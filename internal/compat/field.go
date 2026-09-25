package compat

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// compareMessages diffs two versions of the same fully-qualified message.
func (c *checker) compareMessages(old, new protoreflect.MessageDescriptor) {
	fqn := string(old.FullName())

	oldFields := fieldsByNumber(old)
	newFields := fieldsByNumber(new)

	// Reserved ranges/names of the opposite side.
	newReservedNums, newReservedNames := new.ReservedRanges(), new.ReservedNames()
	oldReservedNums, oldReservedNames := old.ReservedRanges(), old.ReservedNames()

	// Fields whose number vanished from the new schema.
	for num, ofd := range oldFields {
		if _, ok := newFields[num]; ok {
			continue
		}
		path := string(ofd.Name())
		switch {
		case hasNumber(newReservedNums, num) && hasName(newReservedNames, ofd.Name()):
			c.add("FIELD_REMOVED_RESERVED", SeverityInfo, DimensionBoth, fqn, path,
				fmt.Sprintf("field %s (#%d) deleted and both number and name reserved — the correct way to delete",
					ofd.Name(), num))
		case hasNumber(newReservedNums, num):
			c.add("FIELD_REMOVED_NAME_UNRESERVED", SeverityWarn, DimensionJSON, fqn, path,
				fmt.Sprintf("field %s (#%d) deleted: number reserved but JSON name %q is not; a future field may reuse the JSON name and break JSON consumers",
					ofd.Name(), num, ofd.JSONName()))
		default:
			c.add("FIELD_REMOVED_UNRESERVED", SeverityFail, DimensionWire, fqn, path,
				fmt.Sprintf("field %s (#%d) deleted without reserving the number; a future field may reuse #%d and silently misread old payloads",
					ofd.Name(), num, num))
		}
	}

	// Fields whose number is new.
	for num, nfd := range newFields {
		if _, ok := oldFields[num]; ok {
			continue
		}
		path := string(nfd.Name())
		switch {
		case hasNumber(oldReservedNums, num):
			c.add("FIELD_USES_RESERVED_NUMBER", SeverityWarn, DimensionWire, fqn, path,
				fmt.Sprintf("field %s reuses number #%d that was explicitly reserved; if it was reserved after a historical deletion, old payloads may be misread",
					nfd.Name(), num))
		default:
			c.add("FIELD_ADDED", SeverityInfo, DimensionBoth, fqn, path,
				fmt.Sprintf("field %s (#%d) added", nfd.Name(), num))
		}
		if hasName(oldReservedNames, nfd.Name()) {
			c.add("FIELD_USES_RESERVED_NAME", SeverityWarn, DimensionJSON, fqn, path,
				fmt.Sprintf("field %s reuses a name that was explicitly reserved; JSON consumers may confuse it with the historical field",
					nfd.Name()))
		}
	}

	// Fields present under the same number in both.
	for num, ofd := range oldFields {
		nfd, ok := newFields[num]
		if !ok {
			continue
		}
		c.compareField(fqn, num, ofd, nfd)
	}

	// Reserved ranges themselves: un-reserving a range without reusing it
	// still cannot be proven safe against historical deletions.
	c.compareReservedRanges(fqn, "message", oldReservedNums, newReservedNums)

	// Oneof-level observations.
	c.compareOneofs(fqn, old, new)
}

// compareField handles two fields sharing the same number.
func (c *checker) compareField(fqn string, num protoreflect.FieldNumber, ofd, nfd protoreflect.FieldDescriptor) {
	path := string(ofd.Name())

	// Same number, different name: rename or reuse? If the kind also
	// changed it is provably a different field (reuse). With identical
	// kind we cannot distinguish a rename from a semantic swap, so wire
	// stays WARN; JSON is decided by the effective JSON name.
	if ofd.Name() != nfd.Name() {
		if ofd.Kind() != nfd.Kind() || ofd.IsList() != nfd.IsList() || ofd.IsMap() != nfd.IsMap() {
			c.add("FIELD_NUMBER_REUSED", SeverityFail, DimensionBoth, fqn, path,
				fmt.Sprintf("number #%d changed from %s (%s) to %s (%s): the number now denotes a different field",
					num, ofd.Name(), kindLabel(ofd), nfd.Name(), kindLabel(nfd)))
			return
		}
		if ofd.JSONName() != nfd.JSONName() {
			c.add("FIELD_RENAMED", SeverityFail, DimensionJSON, fqn, path,
				fmt.Sprintf("field #%d renamed %s -> %s: JSON name %q -> %q breaks JSON consumers",
					num, ofd.Name(), nfd.Name(), ofd.JSONName(), nfd.JSONName()))
		} else {
			c.add("FIELD_RENAMED", SeverityWarn, DimensionWire, fqn, path,
				fmt.Sprintf("field #%d renamed %s -> %s with JSON name kept as %q; wire-compatible, but descriptors cannot prove it is a rename rather than a semantic swap",
					num, ofd.Name(), nfd.Name(), ofd.JSONName()))
		}
		return
	}

	// Same name and number from here on.
	if ofd.JSONName() != nfd.JSONName() {
		c.add("FIELD_JSON_NAME_CHANGED", SeverityFail, DimensionJSON, fqn, path,
			fmt.Sprintf("field %s changed JSON name %q -> %q", ofd.Name(), ofd.JSONName(), nfd.JSONName()))
	}

	c.compareFieldType(fqn, path, ofd, nfd)
	c.compareCardinality(fqn, path, ofd, nfd)
	c.compareOneofMembership(fqn, path, ofd, nfd)
	c.compareDefault(fqn, path, ofd, nfd)
	c.compareRequired(fqn, path, ofd, nfd)
}

// compareFieldType checks kind and referenced type changes.
func (c *checker) compareFieldType(fqn, path string, ofd, nfd protoreflect.FieldDescriptor) {
	// Maps: compare key and value kinds; any change reshapes both encodings.
	if ofd.IsMap() || nfd.IsMap() {
		if ofd.IsMap() != nfd.IsMap() {
			c.add("FIELD_TYPE_CHANGED", SeverityFail, DimensionBoth, fqn, path,
				fmt.Sprintf("field %s changed between map and non-map", ofd.Name()))
			return
		}
		ok, nv := ofd.MapKey(), nfd.MapKey()
		ov, nvv := ofd.MapValue(), nfd.MapValue()
		if ok.Kind() != nv.Kind() || ov.Kind() != nvv.Kind() {
			c.add("FIELD_TYPE_CHANGED", SeverityFail, DimensionBoth, fqn, path,
				fmt.Sprintf("map field %s changed from map<%s, %s> to map<%s, %s>",
					ofd.Name(), ok.Kind(), ov.Kind(), nv.Kind(), nvv.Kind()))
		}
		return
	}

	if ofd.Kind() == nfd.Kind() {
		switch ofd.Kind() {
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if ofd.Message().FullName() != nfd.Message().FullName() {
				c.add("FIELD_MESSAGE_TYPE_CHANGED", SeverityFail, DimensionBoth, fqn, path,
					fmt.Sprintf("field %s changed message type %s -> %s; both are length-delimited on the wire but the payload would be reinterpreted",
						ofd.Name(), ofd.Message().FullName(), nfd.Message().FullName()))
			}
		case protoreflect.EnumKind:
			if ofd.Enum().FullName() != nfd.Enum().FullName() {
				c.compareEnumSwap(fqn, path, ofd, nfd)
			}
		}
		return
	}

	// Kind changed: classify wire compatibility and JSON representation.
	wireSev, wireNote := wireTypeChange(ofd, nfd)
	jsonSev, jsonNote := jsonTypeChange(ofd, nfd)

	sev := wireSev
	dim := DimensionWire
	if jsonSev == SeverityFail {
		sev, dim = SeverityFail, DimensionBoth
	} else if wireSev == SeverityFail {
		dim = DimensionWire
	} else if jsonSev == SeverityWarn || wireSev == SeverityWarn {
		sev, dim = SeverityWarn, DimensionBoth
	} else {
		sev, dim = SeverityInfo, DimensionBoth
	}
	c.add("FIELD_TYPE_CHANGED", sev, dim, fqn, path,
		fmt.Sprintf("field %s changed type %s -> %s. wire: %s; json: %s",
			ofd.Name(), kindLabel(ofd), kindLabel(nfd), wireNote, jsonNote))
}

// compareEnumSwap handles a field whose enum type was swapped for another.
func (c *checker) compareEnumSwap(fqn, path string, ofd, nfd protoreflect.FieldDescriptor) {
	oldEnum, newEnum := ofd.Enum(), nfd.Enum()
	compatible := true
	vals := oldEnum.Values()
	for i := 0; i < vals.Len(); i++ {
		v := vals.Get(i)
		nv := newEnum.Values().ByNumber(v.Number())
		if nv == nil || nv.Name() != v.Name() {
			compatible = false
			break
		}
	}
	if compatible {
		c.add("FIELD_ENUM_TYPE_CHANGED", SeverityWarn, DimensionBoth, fqn, path,
			fmt.Sprintf("field %s changed enum type %s -> %s; all old values exist with identical names/numbers, but the type identity changed and generated code will differ",
				ofd.Name(), oldEnum.FullName(), newEnum.FullName()))
	} else {
		c.add("FIELD_ENUM_TYPE_CHANGED", SeverityFail, DimensionBoth, fqn, path,
			fmt.Sprintf("field %s changed enum type %s -> %s and the value sets differ",
				ofd.Name(), oldEnum.FullName(), newEnum.FullName()))
	}
}

// compareCardinality checks singular/repeated transitions.
func (c *checker) compareCardinality(fqn, path string, ofd, nfd protoreflect.FieldDescriptor) {
	if ofd.IsList() == nfd.IsList() {
		// Both repeated: packedness flips are safe because parsers must
		// accept both encodings; JSON shape is unchanged.
		if ofd.IsList() && ofd.IsPacked() != nfd.IsPacked() {
			c.add("FIELD_PACKED_CHANGED", SeverityInfo, DimensionBoth, fqn, path,
				fmt.Sprintf("repeated field %s changed packed=%v -> %v; readers must accept both encodings",
					ofd.Name(), ofd.IsPacked(), nfd.IsPacked()))
		}
		return
	}
	packable := isPackable(ofd.Kind())
	switch {
	case !ofd.IsList() && nfd.IsList():
		// Singular -> repeated: old payloads hold at most one element and
		// repeated readers accept unpacked data. JSON shape changes.
		c.add("FIELD_CARDINALITY_CHANGED", SeverityFail, DimensionJSON, fqn, path,
			fmt.Sprintf("field %s changed singular -> repeated; wire data stays readable, but JSON changes from value to array",
				ofd.Name()))
	case ofd.IsList() && !nfd.IsList():
		// Repeated -> singular: packed data is unreadable by a singular
		// scalar reader; unpacked data collapses to last-one-wins.
		switch {
		case packable && ofd.IsPacked():
			c.add("FIELD_CARDINALITY_CHANGED", SeverityFail, DimensionBoth, fqn, path,
				fmt.Sprintf("field %s changed packed repeated -> singular; existing packed payloads become unknown fields to new readers (silent data loss)",
					ofd.Name()))
		default:
			c.add("FIELD_CARDINALITY_CHANGED", SeverityWarn, DimensionBoth, fqn, path,
				fmt.Sprintf("field %s changed repeated -> singular; multi-element payloads collapse to the last element (messages merge), and JSON changes from array to value",
					ofd.Name()))
		}
	}
}

// compareOneofMembership checks a field moving in or out of a oneof.
// Synthetic oneofs (proto3 optional) are ignored.
func (c *checker) compareOneofMembership(fqn, path string, ofd, nfd protoreflect.FieldDescriptor) {
	oo, no := realOneof(ofd), realOneof(nfd)
	switch {
	case oo == nil && no == nil:
	case oo == nil && no != nil:
		c.add("FIELD_MOVED_INTO_ONEOF", SeverityWarn, DimensionBoth, fqn, path,
			fmt.Sprintf("field %s moved into oneof %q; old payloads may set it together with siblings that now mutually exclude, and JSON documents setting two members fail to parse",
				ofd.Name(), no.Name()))
	case oo != nil && no == nil:
		c.add("FIELD_MOVED_OUT_OF_ONEOF", SeverityInfo, DimensionBoth, fqn, path,
			fmt.Sprintf("field %s moved out of oneof %q; old payloads remain readable, generated APIs change",
				ofd.Name(), oo.Name()))
	case oo.Name() != no.Name():
		c.add("FIELD_MOVED_BETWEEN_ONEOFS", SeverityWarn, DimensionBoth, fqn, path,
			fmt.Sprintf("field %s moved from oneof %q to oneof %q", ofd.Name(), oo.Name(), no.Name()))
	}
}

// compareOneofs reports oneofs that disappeared or appeared.
func (c *checker) compareOneofs(fqn string, old, new protoreflect.MessageDescriptor) {
	oldOnes := realOneofs(old)
	newOnes := realOneofs(new)
	for name, oo := range oldOnes {
		if _, ok := newOnes[name]; !ok {
			c.add("ONEOF_REMOVED", SeverityWarn, DimensionBoth, fqn, string(oo.Name()),
				fmt.Sprintf("oneof %q no longer exists; its %d member(s) became independent fields or were removed",
					oo.Name(), oo.Fields().Len()))
		}
	}
	for name := range newOnes {
		if _, ok := oldOnes[name]; !ok {
			c.add("ONEOF_ADDED", SeverityInfo, DimensionBoth, fqn, name,
				fmt.Sprintf("oneof %q is new", name))
		}
	}
}

// compareDefault checks proto2 explicit default changes.
func (c *checker) compareDefault(fqn, path string, ofd, nfd protoreflect.FieldDescriptor) {
	if !ofd.HasDefault() && !nfd.HasDefault() {
		return
	}
	if ofd.HasDefault() != nfd.HasDefault() || !valuesEqual(ofd.Default(), nfd.Default()) {
		c.add("FIELD_DEFAULT_CHANGED", SeverityWarn, DimensionBoth, fqn, path,
			fmt.Sprintf("field %s changed explicit default %q -> %q; readers of old data observe the default when the field is absent",
				ofd.Name(), formatValue(ofd, ofd.Default()), formatValue(nfd, nfd.Default())))
	}
}

// compareRequired checks proto2 required/optional transitions.
func (c *checker) compareRequired(fqn, path string, ofd, nfd protoreflect.FieldDescriptor) {
	oldReq := ofd.Cardinality() == protoreflect.Required
	newReq := nfd.Cardinality() == protoreflect.Required
	switch {
	case !oldReq && newReq:
		c.add("FIELD_BECAME_REQUIRED", SeverityFail, DimensionWire, fqn, path,
			fmt.Sprintf("field %s became required; existing payloads without it fail proto2 validation", ofd.Name()))
	case oldReq && !newReq:
		c.add("FIELD_BECAME_OPTIONAL", SeverityInfo, DimensionWire, fqn, path,
			fmt.Sprintf("field %s relaxed from required to optional", ofd.Name()))
	}
}

// compareReservedRanges flags reserved ranges that silently disappeared.
func (c *checker) compareReservedRanges(fqn, kind string, oldR, newR protoreflect.FieldRanges) {
	for i := 0; i < oldR.Len(); i++ {
		r := oldR.Get(i)
		if !rangeCovered(newR, r) {
			c.add("RESERVED_RANGE_REMOVED", SeverityWarn, DimensionWire, fqn, "",
				fmt.Sprintf("%s reserved range [%d, %d] was un-reserved; if it protected a historical deletion, old payloads may be misread",
					kind, r[0], r[1]-1))
		}
	}
}

// --- helpers ---

func fieldsByNumber(md protoreflect.MessageDescriptor) map[protoreflect.FieldNumber]protoreflect.FieldDescriptor {
	out := map[protoreflect.FieldNumber]protoreflect.FieldDescriptor{}
	fs := md.Fields()
	for i := 0; i < fs.Len(); i++ {
		f := fs.Get(i)
		out[f.Number()] = f
	}
	return out
}

func hasNumber(ranges protoreflect.FieldRanges, num protoreflect.FieldNumber) bool {
	for i := 0; i < ranges.Len(); i++ {
		r := ranges.Get(i)
		if num >= r[0] && num < r[1] {
			return true
		}
	}
	return false
}

func hasName(names protoreflect.Names, name protoreflect.Name) bool {
	for i := 0; i < names.Len(); i++ {
		if names.Get(i) == name {
			return true
		}
	}
	return false
}

func rangeCovered(ranges protoreflect.FieldRanges, r [2]protoreflect.FieldNumber) bool {
	// Every number in r must be covered by some range in ranges.
	for n := r[0]; n < r[1]; n++ {
		if !hasNumber(ranges, n) {
			return false
		}
	}
	return true
}

func realOneof(fd protoreflect.FieldDescriptor) protoreflect.OneofDescriptor {
	od := fd.ContainingOneof()
	if od == nil || od.IsSynthetic() {
		return nil
	}
	return od
}

func realOneofs(md protoreflect.MessageDescriptor) map[string]protoreflect.OneofDescriptor {
	out := map[string]protoreflect.OneofDescriptor{}
	ods := md.Oneofs()
	for i := 0; i < ods.Len(); i++ {
		od := ods.Get(i)
		if !od.IsSynthetic() {
			out[string(od.Name())] = od
		}
	}
	return out
}

func isPackable(k protoreflect.Kind) bool {
	switch k {
	case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.MessageKind, protoreflect.GroupKind:
		return false
	}
	return true
}

func kindLabel(fd protoreflect.FieldDescriptor) string {
	label := fd.Kind().String()
	if fd.IsMap() {
		label = "map<" + fd.MapKey().Kind().String() + ", " + fd.MapValue().Kind().String() + ">"
	}
	if fd.IsList() {
		label = "repeated " + label
	}
	return label
}

func valuesEqual(a, b protoreflect.Value) bool {
	return a.Interface() == b.Interface()
}
