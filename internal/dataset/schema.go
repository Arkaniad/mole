// Package dataset is §13's dataset output mode: a table instead of prose.
//
// Schema, then per-lead row extraction, then a cross-source merge by fuzzy key,
// then CSV or JSON. Each part is separated by how confidently it can be judged. A
// schema is a declaration and can be validated. An extracted row carries a
// verbatim quote and dies the same way a fabricated claim does (§11.5). A MERGE is
// a judgement, so it is the one part with precision and recall attached to it.
//
// This deliberately does not scrape. Rows come from the same fetch-and-extract
// path the web actor already uses; for bulk structured extraction at scale, an
// official API is the right tool.
package dataset

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ErrSchema matches every schema problem, so a caller can tell a bad schema from
// a failed extraction.
var ErrSchema = errors.New("dataset: invalid schema")

func schemaErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSchema, fmt.Sprintf(format, args...))
}

// FieldType is what a column holds.
//
// Three types, not a type system. The point of declaring one is to tell the
// extracting model what to look for and to let the merge normalise before
// comparing — a date written "3 March 2024" and "2024-03-03" are the same key
// only if something knows the field is a date. Anything finer would be a
// promise about the world that a web page cannot keep.
type FieldType string

const (
	TypeText   FieldType = "text"
	TypeNumber FieldType = "number"
	TypeDate   FieldType = "date"
)

func (t FieldType) valid() bool {
	switch t {
	case TypeText, TypeNumber, TypeDate:
		return true
	}
	return false
}

// Field is one column.
type Field struct {
	Name string    `json:"name"`
	Type FieldType `json:"type"`
	// Description is what the extracting model is told to look for. The single
	// most useful thing a user can supply: "revenue" finds three different
	// numbers on a page, "annual revenue in USD for the most recent full
	// financial year" finds one.
	Description string `json:"description,omitempty"`
	// Key marks a field that identifies the entity a row is about.
	//
	// At least one is required, and the requirement is not bureaucratic: a
	// dataset with no key cannot be merged, and merging across sources is what
	// distinguishes this from a list of quotes. A schema with no key would
	// silently degrade into the latter.
	Key bool `json:"key,omitempty"`
}

// Schema is the shape of the dataset.
type Schema struct {
	// Name is carried through a schema file so one can be labelled, and is used
	// by nothing. It used to claim it named the output file, which no code did.
	Name   string  `json:"name,omitempty"`
	Fields []Field `json:"fields"`
}

// fieldName is what a column may be called.
//
// Deliberately narrow. A field name reaches a CSV header, a JSON key, a model
// prompt and a merge comparison, and the connector's experience with headers
// (M8) is the argument for constraining it at the door rather than escaping it
// four times.
var fieldName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Validate checks the schema is usable before anything is spent against it.
func (s Schema) Validate() error {
	if len(s.Fields) == 0 {
		return schemaErr("no fields")
	}
	if len(s.Fields) > MaxFields {
		return schemaErr("%d fields, limit %d; a wider schema is a worse extraction "+
			"rather than a richer one", len(s.Fields), MaxFields)
	}

	seen := map[string]bool{}
	var keys int
	for i, f := range s.Fields {
		if !fieldName.MatchString(f.Name) {
			return schemaErr("field %d is named %q; use lower-case letters, digits and "+
				"underscores, starting with a letter", i+1, f.Name)
		}
		if seen[f.Name] {
			return schemaErr("two fields named %q", f.Name)
		}
		if reservedColumn(f.Name) {
			return schemaErr("field %q collides with a column the CSV output adds "+
				"(%s); a reader keyed by header name would silently take the wrong one",
				f.Name, strings.Join(ReservedColumns[:], ", "))
		}
		seen[f.Name] = true
		if !f.Type.valid() {
			return schemaErr("field %q has type %q; want text, number or date",
				f.Name, f.Type)
		}
		if f.Key {
			keys++
		}
	}
	if keys == 0 {
		return schemaErr("no key field; mark at least one field with a trailing ! " +
			"(name:type!) — without a key, rows from different sources cannot be " +
			"merged and the result is a list of quotes rather than a dataset")
	}
	return nil
}

func reservedColumn(name string) bool {
	for _, r := range ReservedColumns {
		if name == r {
			return true
		}
	}
	return false
}

// MaxFields bounds the schema.
//
// A wide schema is a worse extraction, not a richer one: every field is another
// thing the model is asked to find in one chunk, and the ones it cannot find
// come back empty or invented. Twenty is generous for the shape of question
// dataset mode answers.
const MaxFields = 20

// Keys returns the key field names, in schema order.
func (s Schema) Keys() []string {
	var out []string
	for _, f := range s.Fields {
		if f.Key {
			out = append(out, f.Name)
		}
	}
	return out
}

// Field looks up a field by name.
func (s Schema) Field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// Names returns every field name in order.
func (s Schema) Names() []string {
	out := make([]string, 0, len(s.Fields))
	for _, f := range s.Fields {
		out = append(out, f.Name)
	}
	return out
}

// -----------------------------------------------------------------------------
// Parsing
// -----------------------------------------------------------------------------

// ParseSpec reads the compact command-line form:
//
//	company:text!,revenue:number,founded:date
//
// A trailing `!` marks a key field. A `=` supplies a description:
//
//	revenue:number=annual revenue in USD for the last full financial year
//
// The compact form exists because the alternative is a file for every
// three-column question, and the file form is still there for anything longer.
func ParseSpec(spec string) (Schema, error) {
	var s Schema
	for _, part := range splitFields(spec) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f, err := parseField(part)
		if err != nil {
			return Schema{}, err
		}
		s.Fields = append(s.Fields, f)
	}
	if err := s.Validate(); err != nil {
		return Schema{}, err
	}
	return s, nil
}

// splitFields splits on commas that separate FIELDS rather than commas inside a
// description.
//
// A description is free text after `=`, and "revenue:number=annual revenue, in
// USD" was being split into a second field called "in USD" — advertising free
// text and then refusing the commonest thing free text contains. A comma after an
// `=` belongs to the description until the next `name:type` looks like one.
func splitFields(spec string) []string {
	var out []string
	var cur strings.Builder
	inDescription := false
	for _, part := range strings.Split(spec, ",") {
		looksLikeField := strings.Contains(part, ":") &&
			!strings.Contains(strings.SplitN(part, ":", 2)[0], " ")
		switch {
		case !inDescription || looksLikeField:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			cur.WriteString(part)
			inDescription = strings.Contains(part, "=")
		default:
			cur.WriteString(",")
			cur.WriteString(part)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func parseField(part string) (Field, error) {
	var f Field
	// The description is split off first: it is free text and may contain the
	// colon that separates name from type.
	if eq := strings.Index(part, "="); eq >= 0 {
		f.Description = strings.TrimSpace(part[eq+1:])
		part = part[:eq]
	}
	name, typ, ok := strings.Cut(strings.TrimSpace(part), ":")
	if !ok {
		return Field{}, schemaErr("field %q has no type; write name:type", part)
	}
	typ = strings.TrimSpace(typ)
	if strings.HasSuffix(typ, "!") {
		f.Key = true
		typ = strings.TrimSuffix(typ, "!")
	}
	f.Name = strings.ToLower(strings.TrimSpace(name))
	f.Type = FieldType(strings.ToLower(typ))
	return f, nil
}

// LoadSchema reads a schema from a JSON file.
func LoadSchema(path string) (Schema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Schema{}, fmt.Errorf("dataset: read schema: %w", err)
	}
	var s Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return Schema{}, schemaErr("parse %s: %s", path, err)
	}
	for i := range s.Fields {
		s.Fields[i].Name = strings.ToLower(strings.TrimSpace(s.Fields[i].Name))
		s.Fields[i].Type = FieldType(strings.ToLower(string(s.Fields[i].Type)))
	}
	if err := s.Validate(); err != nil {
		return Schema{}, err
	}
	return s, nil
}

// Describe renders the schema for a prompt.
//
// The field list a model is asked to fill, with the types and descriptions,
// because a type alone tells it almost nothing: "founded: date" and "founded:
// date — the year the company was incorporated" produce different extractions
// from the same page.
func (s Schema) Describe() string {
	var b strings.Builder
	for _, f := range s.Fields {
		fmt.Fprintf(&b, "- %s (%s)", f.Name, f.Type)
		if f.Key {
			b.WriteString(" [identifies the row]")
		}
		if f.Description != "" {
			b.WriteString(" — " + f.Description)
		}
		b.WriteString("\n")
	}
	return b.String()
}
