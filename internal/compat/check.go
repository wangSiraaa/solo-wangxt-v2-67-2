package compat

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Input configures one comparison.
type Input struct {
	Old *protoregistry.Files
	New *protoregistry.Files
	// OwnedPaths limits diffing to these file paths (the package's own
	// files). Well-known types under google/protobuf/ are always skipped.
	// If empty, every non-well-known file is compared.
	OwnedPaths []string
	// Samples are optional payloads re-parsed under both schemas.
	Samples []Sample
}

// Check compares old and new descriptor closures and returns the evidence.
func Check(in Input) *Report {
	c := &checker{
		old:   in.Old,
		new:   in.New,
		owned: make(map[string]bool, len(in.OwnedPaths)),
	}
	for _, p := range in.OwnedPaths {
		c.owned[p] = true
	}
	c.compareFiles()

	rep := &Report{}
	if len(in.Samples) > 0 {
		rep.Samples = VerifySamples(in.Old, in.New, in.Samples)
		c.findings = append(c.findings, sampleFindings(rep.Samples)...)
	}
	rep.Findings = c.findings
	rep.Verdict = verdictOf(c.findings)
	return rep
}

type checker struct {
	old, new *protoregistry.Files
	owned    map[string]bool
	findings []Finding
}

func (c *checker) add(code string, sev Severity, dim Dimension, msg, path, detail string) {
	c.findings = append(c.findings, Finding{
		Code: code, Severity: sev, Dimension: dim,
		Message: msg, Path: path, Detail: detail,
	})
}

// compareFiles walks every owned old file and matches it against the new
// set by path; members are then matched by fully-qualified name, so files
// may be reorganized and same-named messages in different packages never
// conflate.
func (c *checker) compareFiles() {
	c.old.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !c.isOwned(fd.Path()) {
			return true
		}
		newFd, err := c.new.FindFileByPath(fd.Path())
		if err != nil {
			c.add("FILE_REMOVED", SeverityWarn, DimensionBoth, "", "",
				fmt.Sprintf("file %s existed before and is gone; its members are reported below", fd.Path()))
			c.walkRemovedFile(fd)
			return true
		}
		if fd.Package() != newFd.Package() {
			c.add("PACKAGE_CHANGED", SeverityWarn, DimensionBoth, "", "",
				fmt.Sprintf("file %s moved from package %q to %q; members are matched by fully-qualified name",
					fd.Path(), fd.Package(), newFd.Package()))
		}
		c.walkMessages(fd.Messages())
		c.walkEnums(fd.Enums())
		return true
	})
	// Files only present in the new set are purely additive.
	c.new.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !c.isOwned(fd.Path()) {
			return true
		}
		if _, err := c.old.FindFileByPath(fd.Path()); err != nil {
			c.add("FILE_ADDED", SeverityInfo, DimensionBoth, "", "",
				fmt.Sprintf("file %s is new", fd.Path()))
		}
		return true
	})
}

func (c *checker) isOwned(path string) bool {
	if strings.HasPrefix(path, "google/protobuf/") {
		return false
	}
	if len(c.owned) == 0 {
		return true
	}
	return c.owned[path]
}

func (c *checker) walkRemovedFile(fd protoreflect.FileDescriptor) {
	msgs := fd.Messages()
	for i := 0; i < msgs.Len(); i++ {
		c.reportRemovedMessage(msgs.Get(i))
	}
	enums := fd.Enums()
	for i := 0; i < enums.Len(); i++ {
		ed := enums.Get(i)
		c.add("ENUM_REMOVED", SeverityWarn, DimensionBoth, string(ed.FullName()), "",
			fmt.Sprintf("enum %s removed together with file %s; cannot prove no consumer remains",
				ed.FullName(), fd.Path()))
	}
}

func (c *checker) walkMessages(list protoreflect.MessageDescriptors) {
	for i := 0; i < list.Len(); i++ {
		md := list.Get(i)
		// Map entry types are compiler-generated; they are checked via
		// their parent map field.
		if md.IsMapEntry() {
			continue
		}
		newMd := findMessage(c.new, md.FullName())
		if newMd == nil {
			c.reportRemovedMessage(md)
		} else {
			c.compareMessages(md, newMd)
		}
		c.walkMessages(md.Messages())
		c.walkEnums(md.Enums())
	}
}

func (c *checker) reportRemovedMessage(md protoreflect.MessageDescriptor) {
	fqn := string(md.FullName())
	c.add("MESSAGE_REMOVED", SeverityWarn, DimensionBoth, fqn, "",
		fmt.Sprintf("message %s was removed; descriptor evidence cannot prove no stored data or consumer remains", fqn))
	// Nested types disappear with it.
	nested := md.Messages()
	for i := 0; i < nested.Len(); i++ {
		if !nested.Get(i).IsMapEntry() {
			c.reportRemovedMessage(nested.Get(i))
		}
	}
	enums := md.Enums()
	for i := 0; i < enums.Len(); i++ {
		ed := enums.Get(i)
		c.add("ENUM_REMOVED", SeverityWarn, DimensionBoth, string(ed.FullName()), "",
			fmt.Sprintf("enum %s removed together with message %s", ed.FullName(), fqn))
	}
}

func (c *checker) walkEnums(list protoreflect.EnumDescriptors) {
	for i := 0; i < list.Len(); i++ {
		ed := list.Get(i)
		newEd := findEnum(c.new, ed.FullName())
		if newEd == nil {
			c.add("ENUM_REMOVED", SeverityWarn, DimensionBoth, string(ed.FullName()), "",
				fmt.Sprintf("enum %s was removed; cannot prove no consumer remains", ed.FullName()))
			continue
		}
		c.compareEnums(ed, newEd)
	}
}

func findMessage(files *protoregistry.Files, name protoreflect.FullName) protoreflect.MessageDescriptor {
	d, err := files.FindDescriptorByName(name)
	if err != nil {
		return nil
	}
	md, ok := d.(protoreflect.MessageDescriptor)
	if !ok || md.IsMapEntry() {
		return nil
	}
	return md
}

func findEnum(files *protoregistry.Files, name protoreflect.FullName) protoreflect.EnumDescriptor {
	d, err := files.FindDescriptorByName(name)
	if err != nil {
		return nil
	}
	ed, ok := d.(protoreflect.EnumDescriptor)
	if !ok {
		return nil
	}
	return ed
}
