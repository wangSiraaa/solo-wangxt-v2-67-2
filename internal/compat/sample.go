package compat

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

const maxDiffsPerSample = 32

// VerifySamples parses each payload under both the old and the new schema
// and compares the results. A sample proves a break when the two parses
// disagree; it proves nothing when it cannot be parsed at all.
func VerifySamples(oldFiles, newFiles *protoregistry.Files, samples []Sample) []SampleResult {
	results := make([]SampleResult, 0, len(samples))
	for _, s := range samples {
		results = append(results, verifySample(oldFiles, newFiles, s))
	}
	return results
}

func verifySample(oldFiles, newFiles *protoregistry.Files, s Sample) SampleResult {
	res := SampleResult{Name: s.Name, Message: s.Message, Encoding: s.Encoding}

	mdOld := findMessage(oldFiles, protoreflect.FullName(s.Message))
	if mdOld == nil {
		res.Status = "ERROR"
		res.Error = fmt.Sprintf("message %s not found in the old schema", s.Message)
		return res
	}
	mdNew := findMessage(newFiles, protoreflect.FullName(s.Message))
	if mdNew == nil {
		res.Status = "ERROR"
		res.Error = fmt.Sprintf("message %s not found in the new schema", s.Message)
		return res
	}

	var payload []byte
	switch s.Encoding {
	case "json":
		payload = []byte(s.Data)
	case "wire":
		b, err := base64.StdEncoding.DecodeString(s.Data)
		if err != nil {
			res.Status = "ERROR"
			res.Error = "wire sample is not valid base64: " + err.Error()
			return res
		}
		payload = b
	default:
		res.Status = "ERROR"
		res.Error = fmt.Sprintf("unknown encoding %q (want json or wire)", s.Encoding)
		return res
	}

	mOld := dynamicpb.NewMessage(mdOld)
	if err := unmarshal(s.Encoding, payload, mOld); err != nil {
		res.Status = "ERROR"
		res.Error = fmt.Sprintf("payload does not parse under the old schema: %v", err)
		return res
	}
	mNew := dynamicpb.NewMessage(mdNew)
	if err := unmarshal(s.Encoding, payload, mNew); err != nil {
		res.Status = "PARSE_ERROR"
		res.Error = fmt.Sprintf("payload parses under the old schema but fails under the new schema: %v", err)
		return res
	}

	res.WireEqual = canonicalWire(mOld) == canonicalWire(mNew)
	oldJSON := canonicalJSON(mOld)
	newJSON := canonicalJSON(mNew)
	res.JSONEqual = oldJSON == newJSON

	if !res.WireEqual || !res.JSONEqual {
		res.Status = "MISMATCH"
		diffs := make([]Diff, 0, 8)
		diffMessages(mOld, mNew, s.Message, &diffs)
		res.Diffs = diffs
		return res
	}
	res.Status = "OK"
	return res
}

func unmarshal(encoding string, payload []byte, m proto.Message) error {
	if encoding == "json" {
		return protojson.UnmarshalOptions{AllowPartial: true, DiscardUnknown: false}.Unmarshal(payload, m)
	}
	return proto.UnmarshalOptions{AllowPartial: true}.Unmarshal(payload, m)
}

// canonicalWire renders a parsed message deterministically. For JSON
// samples the wire form is still derived, so both dimensions are checked
// for every sample.
func canonicalWire(m proto.Message) string {
	b, err := proto.MarshalOptions{Deterministic: true, AllowPartial: true}.Marshal(m)
	if err != nil {
		return "<marshal error: " + err.Error() + ">"
	}
	return string(b)
}

func canonicalJSON(m proto.Message) string {
	b, err := protojson.MarshalOptions{AllowPartial: true}.Marshal(m)
	if err != nil {
		return "<marshal error: " + err.Error() + ">"
	}
	return string(b)
}

// sampleFindings converts sample outcomes into report findings. A sample
// that cannot run is WARN — verification was requested and could not be
// completed, which must not count as a pass.
func sampleFindings(results []SampleResult) []Finding {
	var out []Finding
	add := func(code string, sev Severity, dim Dimension, r SampleResult, detail string) {
		path := ""
		if len(r.Diffs) > 0 {
			path = r.Diffs[0].Path
		}
		out = append(out, Finding{
			Code: code, Severity: sev, Dimension: dim,
			Message: r.Message, Path: path, Detail: detail,
		})
	}
	for _, r := range results {
		label := r.Message
		if r.Name != "" {
			label = r.Name + " (" + r.Message + ")"
		}
		switch r.Status {
		case "PARSE_ERROR":
			add("SAMPLE_PARSE_FAILED", SeverityFail, dimOf(r.Encoding), r,
				fmt.Sprintf("sample %s: %s", label, r.Error))
		case "MISMATCH":
			if !r.WireEqual {
				add("SAMPLE_WIRE_MISMATCH", SeverityFail, DimensionWire, r,
					fmt.Sprintf("sample %s re-serializes to different wire bytes under the new schema", label))
			}
			if !r.JSONEqual {
				add("SAMPLE_JSON_MISMATCH", SeverityFail, DimensionJSON, r,
					fmt.Sprintf("sample %s renders differently in JSON under the new schema", label))
			}
		case "ERROR":
			add("SAMPLE_UNVERIFIABLE", SeverityWarn, dimOf(r.Encoding), r,
				fmt.Sprintf("sample %s could not be verified: %s", label, r.Error))
		}
	}
	return out
}

func dimOf(encoding string) Dimension {
	if encoding == "json" {
		return DimensionJSON
	}
	return DimensionWire
}

// diffMessages walks two parses of the same payload, matching fields by
// number, and records value differences with dotted field paths.
func diffMessages(om, nm protoreflect.Message, path string, diffs *[]Diff) {
	if len(*diffs) >= maxDiffsPerSample {
		return
	}
	nums := map[protoreflect.FieldNumber]bool{}
	om.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		nums[fd.Number()] = true
		return true
	})
	nm.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		nums[fd.Number()] = true
		return true
	})
	ordered := make([]int, 0, len(nums))
	for n := range nums {
		ordered = append(ordered, int(n))
	}
	sort.Ints(ordered)

	for _, n := range ordered {
		num := protoreflect.FieldNumber(n)
		ofd := om.Descriptor().Fields().ByNumber(num)
		nfd := nm.Descriptor().Fields().ByNumber(num)
		switch {
		case ofd != nil && nfd == nil:
			*diffs = append(*diffs, Diff{
				Path: joinPath(path, string(ofd.Name())),
				Old:  formatField(ofd, om.Get(ofd)),
				New:  "<unknown field in new schema>",
			})
		case ofd == nil && nfd != nil:
			*diffs = append(*diffs, Diff{
				Path: joinPath(path, string(nfd.Name())),
				Old:  "<unknown field in old schema>",
				New:  formatField(nfd, nm.Get(nfd)),
			})
		default:
			diffValues(ofd, nfd, om.Get(ofd), nm.Get(nfd), joinPath(path, string(ofd.Name())), diffs)
		}
		if len(*diffs) >= maxDiffsPerSample {
			return
		}
	}

	ou, nu := om.GetUnknown(), nm.GetUnknown()
	if !bytes.Equal(ou, nu) {
		*diffs = append(*diffs, Diff{
			Path: joinPath(path, "~unknown_fields"),
			Old:  fmt.Sprintf("%d bytes", len(ou)),
			New:  fmt.Sprintf("%d bytes", len(nu)),
		})
	}
}

func diffValues(ofd, nfd protoreflect.FieldDescriptor, ov, nv protoreflect.Value, path string, diffs *[]Diff) {
	if len(*diffs) >= maxDiffsPerSample {
		return
	}
	if ofd.IsMap() || nfd.IsMap() {
		if !ofd.IsMap() || !nfd.IsMap() {
			*diffs = append(*diffs, Diff{Path: path, Old: formatField(ofd, ov), New: formatField(nfd, nv)})
			return
		}
		diffMaps(ofd, nfd, ov.Map(), nv.Map(), path, diffs)
		return
	}
	if ofd.IsList() || nfd.IsList() {
		if !ofd.IsList() || !nfd.IsList() {
			*diffs = append(*diffs, Diff{Path: path, Old: formatField(ofd, ov), New: formatField(nfd, nv)})
			return
		}
		diffLists(ofd, nfd, ov.List(), nv.List(), path, diffs)
		return
	}
	if ofd.Kind() != nfd.Kind() {
		*diffs = append(*diffs, Diff{Path: path, Old: formatField(ofd, ov), New: formatField(nfd, nv)})
		return
	}
	if ofd.Kind() == protoreflect.MessageKind || ofd.Kind() == protoreflect.GroupKind {
		diffMessages(ov.Message(), nv.Message(), path, diffs)
		return
	}
	if ofd.Kind() == protoreflect.EnumKind {
		if ov.Enum() != nv.Enum() {
			*diffs = append(*diffs, Diff{Path: path, Old: formatEnum(ofd, ov.Enum()), New: formatEnum(nfd, nv.Enum())})
			return
		}
		// Same number: the JSON identity is the name.
		on, nn := enumName(ofd, ov.Enum()), enumName(nfd, nv.Enum())
		if on != nn {
			*diffs = append(*diffs, Diff{Path: path, Old: on, New: nn})
		}
		return
	}
	if ov.Interface() != nv.Interface() {
		*diffs = append(*diffs, Diff{Path: path, Old: formatField(ofd, ov), New: formatField(nfd, nv)})
	}
}

func diffMaps(ofd, nfd protoreflect.FieldDescriptor, om, nm protoreflect.Map, path string, diffs *[]Diff) {
	keys := map[string]protoreflect.MapKey{}
	om.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		keys[mapKeyString(k)] = k
		return true
	})
	nm.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		if _, ok := keys[mapKeyString(k)]; !ok {
			keys[mapKeyString(k)] = k
		}
		return true
	})
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, ks := range ordered {
		k := keys[ks]
		p := path + "[" + ks + "]"
		ov, nv := om.Get(k), nm.Get(k)
		ovOK, nvOK := om.Has(k), nm.Has(k)
		switch {
		case ovOK && !nvOK:
			*diffs = append(*diffs, Diff{Path: p, Old: formatField(ofd.MapValue(), ov), New: "<absent>"})
		case !ovOK && nvOK:
			*diffs = append(*diffs, Diff{Path: p, Old: "<absent>", New: formatField(nfd.MapValue(), nv)})
		default:
			diffValues(ofd.MapValue(), nfd.MapValue(), ov, nv, p, diffs)
		}
		if len(*diffs) >= maxDiffsPerSample {
			return
		}
	}
}

func diffLists(ofd, nfd protoreflect.FieldDescriptor, ol, nl protoreflect.List, path string, diffs *[]Diff) {
	n := ol.Len()
	if nl.Len() > n {
		n = nl.Len()
	}
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case i >= ol.Len():
			*diffs = append(*diffs, Diff{Path: p, Old: "<absent>", New: formatField(nfd, nl.Get(i))})
		case i >= nl.Len():
			*diffs = append(*diffs, Diff{Path: p, Old: formatField(ofd, ol.Get(i)), New: "<absent>"})
		default:
			diffValues(ofd, nfd, ol.Get(i), nl.Get(i), p, diffs)
		}
		if len(*diffs) >= maxDiffsPerSample {
			return
		}
	}
}

// --- formatting helpers (also used by field-level checks) ---

func joinPath(base, elem string) string {
	if base == "" {
		return elem
	}
	return base + "." + elem
}

func mapKeyString(k protoreflect.MapKey) string {
	return fmt.Sprintf("%v", k.Interface())
}

func formatField(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	switch {
	case fd.IsMap():
		var parts []string
		v.Map().Range(func(k protoreflect.MapKey, val protoreflect.Value) bool {
			parts = append(parts, fmt.Sprintf("%v: %s", k.Interface(), formatValue(fd.MapValue(), val)))
			return len(parts) < 4
		})
		return "{" + strings.Join(parts, ", ") + "}"
	case fd.IsList():
		var parts []string
		l := v.List()
		for i := 0; i < l.Len() && i < 4; i++ {
			parts = append(parts, formatValue(fd, l.Get(i)))
		}
		if l.Len() > 4 {
			parts = append(parts, fmt.Sprintf("... +%d", l.Len()-4))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return formatValue(fd, v)
	}
}

func formatValue(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	switch fd.Kind() {
	case protoreflect.EnumKind:
		return formatEnum(fd, v.Enum())
	case protoreflect.StringKind:
		return fmt.Sprintf("%q", v.String())
	case protoreflect.BytesKind:
		return base64.StdEncoding.EncodeToString(v.Bytes())
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return "{...}"
	default:
		return fmt.Sprintf("%v", v.Interface())
	}
}

func formatEnum(fd protoreflect.FieldDescriptor, num protoreflect.EnumNumber) string {
	name := enumName(fd, num)
	return fmt.Sprintf("%s(%d)", name, int32(num))
}

func enumName(fd protoreflect.FieldDescriptor, num protoreflect.EnumNumber) string {
	if ev := fd.Enum().Values().ByNumber(num); ev != nil {
		return string(ev.Name())
	}
	return fmt.Sprintf("<unknown:%d>", int32(num))
}
