package compat

import (
	"fmt"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// compareEnums diffs two versions of the same fully-qualified enum.
func (c *checker) compareEnums(old, new protoreflect.EnumDescriptor) {
	fqn := string(old.FullName())
	closed := old.Syntax() == protoreflect.Proto2

	oldVals := enumValuesByNumber(old)
	newVals := enumValuesByNumber(new)
	newReservedNums, newReservedNames := new.ReservedRanges(), new.ReservedNames()
	oldReservedNums := old.ReservedRanges()

	// Values whose number vanished.
	for num, ov := range oldVals {
		if _, ok := newVals[num]; ok {
			continue
		}
		path := string(ov.Name())
		switch {
		case hasEnumNumber(newReservedNums, num) && hasName(newReservedNames, ov.Name()):
			c.add("ENUM_VALUE_REMOVED_RESERVED", SeverityInfo, DimensionBoth, fqn, path,
				fmt.Sprintf("enum value %s (%d) deleted with number and name reserved", ov.Name(), num))
		case hasEnumNumber(newReservedNums, num):
			c.add("ENUM_VALUE_REMOVED_NAME_UNRESERVED", SeverityWarn, DimensionJSON, fqn, path,
				fmt.Sprintf("enum value %s (%d) deleted: number reserved but name is not; a future value may reuse the JSON name", ov.Name(), num))
		default:
			c.add("ENUM_VALUE_REMOVED_UNRESERVED", SeverityFail, DimensionWire, fqn, path,
				fmt.Sprintf("enum value %s (%d) deleted without reserving the number; a future value may reuse %d and reinterpret stored data", ov.Name(), num, num))
		}
		if num == 0 {
			c.add("ENUM_DEFAULT_REMOVED", SeverityFail, DimensionBoth, fqn, path,
				fmt.Sprintf("the zero value %s was removed; the zero value is the implicit default for every unset enum field", ov.Name()))
		}
	}

	// Values whose number is new.
	for num, nv := range newVals {
		if _, ok := oldVals[num]; ok {
			continue
		}
		path := string(nv.Name())
		switch {
		case hasEnumNumber(oldReservedNums, num):
			c.add("ENUM_VALUE_USES_RESERVED_NUMBER", SeverityWarn, DimensionWire, fqn, path,
				fmt.Sprintf("enum value %s reuses reserved number %d; if it was reserved after a historical deletion, stored data may be misread", nv.Name(), num))
		case closed:
			// proto2 closed enums: old readers cannot hold the new value
			// and push it into unknown fields.
			c.add("ENUM_VALUE_ADDED", SeverityWarn, DimensionWire, fqn, path,
				fmt.Sprintf("enum value %s (%d) added to a proto2 closed enum; old readers move it to unknown fields and observe the default instead", nv.Name(), num))
		default:
			c.add("ENUM_VALUE_ADDED", SeverityInfo, DimensionWire, fqn, path,
				fmt.Sprintf("enum value %s (%d) added; proto3 open enums carry unknown values safely", nv.Name(), num))
		}
	}

	// Values present under the same number: name is the JSON identity.
	for num, ov := range oldVals {
		nv, ok := newVals[num]
		if !ok {
			continue
		}
		if ov.Name() == nv.Name() {
			continue
		}
		path := string(ov.Name())
		sev := SeverityFail
		dim := DimensionJSON
		note := fmt.Sprintf("enum value %d renamed %s -> %s; the JSON form is the name, so JSON payloads and consumers comparing names break",
			num, ov.Name(), nv.Name())
		if num == 0 {
			note += "; this value is also the implicit default, so default-rendered JSON changes"
		}
		c.add("ENUM_VALUE_RENAMED", sev, dim, fqn, path, note)
	}

	// Default impact: the zero value is the default for every unset field.
	oldZero, oldHasZero := oldVals[0]
	newZero, newHasZero := newVals[0]
	switch {
	case oldHasZero && !newHasZero:
		// Already reported as ENUM_VALUE_REMOVED_* + ENUM_DEFAULT_REMOVED.
	case oldHasZero && newHasZero && oldZero.Name() != newZero.Name():
		c.add("ENUM_DEFAULT_VALUE_CHANGED", SeverityWarn, DimensionJSON, fqn, string(oldZero.Name()),
			fmt.Sprintf("the zero (default) value changed name %s -> %s; unset enum fields now serialize to a different JSON string",
				oldZero.Name(), newZero.Name()))
	}

	// Un-reserving ranges cannot be proven safe.
	for i := 0; i < oldReservedNums.Len(); i++ {
		r := oldReservedNums.Get(i)
		if !enumRangeCovered(new.ReservedRanges(), r) {
			c.add("ENUM_RESERVED_RANGE_REMOVED", SeverityWarn, DimensionWire, fqn, "",
				fmt.Sprintf("enum reserved range [%d, %d] was un-reserved; if it protected a historical deletion, stored data may be misread",
					int32(r[0]), int32(r[1])))
		}
	}
}

func enumValuesByNumber(ed protoreflect.EnumDescriptor) map[protoreflect.EnumNumber]protoreflect.EnumValueDescriptor {
	out := map[protoreflect.EnumNumber]protoreflect.EnumValueDescriptor{}
	vals := ed.Values()
	for i := 0; i < vals.Len(); i++ {
		v := vals.Get(i)
		out[v.Number()] = v
	}
	return out
}

// Enum reserved ranges are inclusive on both ends (unlike message field
// ranges, which are end-exclusive).
func hasEnumNumber(ranges protoreflect.EnumRanges, num protoreflect.EnumNumber) bool {
	for i := 0; i < ranges.Len(); i++ {
		r := ranges.Get(i)
		if num >= r[0] && num <= r[1] {
			return true
		}
	}
	return false
}

func enumRangeCovered(ranges protoreflect.EnumRanges, r [2]protoreflect.EnumNumber) bool {
	for n := r[0]; n <= r[1]; n++ {
		if !hasEnumNumber(ranges, n) {
			return false
		}
	}
	return true
}
