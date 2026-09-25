package compat

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// wireCategory groups kinds by their wire-type framing.
type wireCategory int

const (
	catVarint wireCategory = iota
	catFixed32
	catFixed64
	catLengthDelim
	catGroup
)

func categoryOf(k protoreflect.Kind) wireCategory {
	switch k {
	case protoreflect.BoolKind, protoreflect.EnumKind,
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind:
		return catVarint
	case protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind, protoreflect.FloatKind:
		return catFixed32
	case protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind, protoreflect.DoubleKind:
		return catFixed64
	case protoreflect.GroupKind:
		return catGroup
	default: // string, bytes, message
		return catLengthDelim
	}
}

// wireTypeChange judges a kind change for the binary encoding. It returns
// the severity and an evidence note.
func wireTypeChange(ofd, nfd protoreflect.FieldDescriptor) (Severity, string) {
	ok, nk := ofd.Kind(), nfd.Kind()
	oc, nc := categoryOf(ok), categoryOf(nk)

	if oc != nc {
		return SeverityFail, fmt.Sprintf("%s and %s use different wire types; existing payloads would be mis-framed or dropped", ok, nk)
	}

	// Same wire framing — but framing alone does not prove safety.
	switch {
	case ok == nk:
		return SeverityInfo, "identical kind"
	case isFixedPair(ok, nk):
		return SeverityInfo, fmt.Sprintf("%s and %s share identical fixed-width encoding", ok, nk)
	case oc == catVarint:
		return varintChange(ok, nk)
	case ok == protoreflect.StringKind && nk == protoreflect.BytesKind:
		return SeverityInfo, "string -> bytes keeps the same length-delimited bytes"
	case ok == protoreflect.BytesKind && nk == protoreflect.StringKind:
		return SeverityWarn, "bytes -> string: old payloads may contain invalid UTF-8, which proto3 string parsing rejects"
	case (ok == protoreflect.MessageKind && nk == protoreflect.GroupKind) ||
		(ok == protoreflect.GroupKind && nk == protoreflect.MessageKind):
		return SeverityFail, "message and group use different wire framing"
	default:
		return SeverityWarn, fmt.Sprintf("%s -> %s shares wire framing but value semantics differ; cannot prove safe", ok, nk)
	}
}

// varintChange classifies changes among varint-encoded kinds.
func varintChange(ok, nk protoreflect.Kind) (Severity, string) {
	widening := map[protoreflect.Kind]protoreflect.Kind{
		protoreflect.Int32Kind:  protoreflect.Int64Kind,
		protoreflect.Sint32Kind: protoreflect.Sint64Kind,
		protoreflect.Uint32Kind: protoreflect.Uint64Kind,
	}
	narrowing := map[protoreflect.Kind]protoreflect.Kind{
		protoreflect.Int64Kind:  protoreflect.Int32Kind,
		protoreflect.Sint64Kind: protoreflect.Sint32Kind,
		protoreflect.Uint64Kind: protoreflect.Uint32Kind,
	}
	if widening[ok] == nk {
		return SeverityInfo, fmt.Sprintf("%s -> %s widens within varint; old values remain valid", ok, nk)
	}
	if narrowing[ok] == nk {
		return SeverityWarn, fmt.Sprintf("%s -> %s narrows; stored values may overflow the smaller range", ok, nk)
	}
	if ok == protoreflect.BoolKind || nk == protoreflect.BoolKind {
		return SeverityWarn, fmt.Sprintf("%s -> %s reinterprets boolean/varint semantics", ok, nk)
	}
	if ok == protoreflect.EnumKind || nk == protoreflect.EnumKind {
		return SeverityWarn, fmt.Sprintf("%s -> %s reinterprets enum semantics", ok, nk)
	}
	// e.g. int32 -> uint32, sint32 -> int32: same framing, different
	// value interpretation (sign, zigzag).
	return SeverityWarn, fmt.Sprintf("%s -> %s shares varint framing but reinterprets values (sign/zigzag)", ok, nk)
}

func isFixedPair(a, b protoreflect.Kind) bool {
	pair := func(x, y protoreflect.Kind) bool { return a == x && b == y || a == y && b == x }
	return pair(protoreflect.Fixed32Kind, protoreflect.Sfixed32Kind) ||
		pair(protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind)
}

// jsonRep is the canonical proto-JSON representation class of a kind.
// Two kinds with different classes serialize differently in JSON.
func jsonRep(k protoreflect.Kind) string {
	switch k {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Uint32Kind,
		protoreflect.Sfixed32Kind, protoreflect.Fixed32Kind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return "number"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Uint64Kind,
		protoreflect.Sfixed64Kind, protoreflect.Fixed64Kind:
		return "quoted-number" // 64-bit integers are JSON strings
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BytesKind:
		return "base64-string"
	case protoreflect.EnumKind:
		return "enum-name"
	default: // message, group
		return "object"
	}
}

// jsonTypeChange judges a kind change for the JSON mapping.
func jsonTypeChange(ofd, nfd protoreflect.FieldDescriptor) (Severity, string) {
	or, nr := jsonRep(ofd.Kind()), jsonRep(nfd.Kind())
	if or == nr {
		return SeverityInfo, fmt.Sprintf("both %s and %s serialize as JSON %s", ofd.Kind(), nfd.Kind(), or)
	}
	return SeverityFail, fmt.Sprintf("JSON representation changes from %s to %s", or, nr)
}
