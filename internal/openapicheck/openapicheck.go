// Package openapicheck checks real HTTP traffic against the OpenAPI document.
// It exists for tests: it is not linked into any binary.
//
// It implements the subset of JSON Schema that api/openapi.yaml uses and, on
// purpose, refuses anything else instead of ignoring it: a validator that
// silently skips a keyword it does not understand would approve everything.
// If the document starts using a new keyword, validation fails with
// "unsupported keyword" and this package must be extended.
package openapicheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// Spec is a parsed OpenAPI document.
type Spec struct {
	doc map[string]any
}

// Load parses an OpenAPI document.
func Load(data []byte) (*Spec, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse openapi document: %w", err)
	}
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.1") {
		return nil, fmt.Errorf("expected an OpenAPI 3.1 document, got %q", v)
	}
	return &Spec{doc: doc}, nil
}

// Operation is one method on one path template, such as "GET /v1/events/{id}".
type Operation struct {
	Method string
	Path   string
}

func (o Operation) String() string { return o.Method + " " + o.Path }

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Operations lists every operation in the document, sorted.
func (s *Spec) Operations() []Operation {
	var ops []Operation
	paths, _ := s.doc["paths"].(map[string]any)
	for path, item := range paths {
		m, _ := item.(map[string]any)
		for _, method := range httpMethods {
			if _, ok := m[method]; ok {
				ops = append(ops, Operation{Method: strings.ToUpper(method), Path: path})
			}
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].String() < ops[j].String() })
	return ops
}

// Statuses returns the response codes an operation declares ("200", "2XX", ...).
func (s *Spec) Statuses(method, path string) ([]string, error) {
	op, err := s.operation(method, path)
	if err != nil {
		return nil, err
	}
	resp, _ := op["responses"].(map[string]any)
	var out []string
	for code := range resp {
		out = append(out, code)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Spec) operation(method, path string) (map[string]any, error) {
	paths, _ := s.doc["paths"].(map[string]any)
	item, ok := paths[path].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the document has no path %q", path)
	}
	op, ok := item[strings.ToLower(method)].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the document has no %s operation on %q", method, path)
	}
	return op, nil
}

// ValidateResponse checks an actual response against what the document
// declares for the operation: the status code must be declared, and a JSON
// body must satisfy the declared schema. A status the document does not list
// is an error, so an undocumented response cannot go unnoticed.
func (s *Spec) ValidateResponse(method, path string, status int, contentType string, body []byte) error {
	op, err := s.operation(method, path)
	if err != nil {
		return err
	}
	responses, _ := op["responses"].(map[string]any)
	decl, found := responses[strconv.Itoa(status)]
	if !found {
		decl, found = responses[fmt.Sprintf("%dXX", status/100)]
	}
	if !found {
		decl, found = responses["default"]
	}
	if !found {
		var declared []string
		for c := range responses {
			declared = append(declared, c)
		}
		sort.Strings(declared)
		return fmt.Errorf("%s %s answered %d, which the document does not declare (declared: %s)", method, path, status, strings.Join(declared, ", "))
	}
	resp, err := s.resolve(decl)
	if err != nil {
		return err
	}

	content, _ := resp["content"].(map[string]any)
	if len(content) == 0 {
		if len(bytes.TrimSpace(body)) != 0 {
			return fmt.Errorf("%s %s %d is declared without a body, but one was sent: %.80s", method, path, status, body)
		}
		return nil
	}
	media := strings.TrimSpace(strings.Split(contentType, ";")[0])
	entry, ok := content[media].(map[string]any)
	if !ok {
		return fmt.Errorf("%s %s %d answered with content type %q, which the document does not declare", method, path, status, media)
	}
	schema, ok := entry["schema"]
	if !ok || media != "application/json" {
		return nil
	}
	return s.validateJSON(schema, body)
}

// ValidateSchema checks a JSON value against components/schemas/<name>.
func (s *Spec) ValidateSchema(name string, body []byte) error {
	return s.validateJSON(map[string]any{"$ref": "#/components/schemas/" + name}, body)
}

// ValidateWebhook checks a request body against what the document declares for
// the webhook, and its headers against the declared required headers.
func (s *Spec) ValidateWebhook(name string, headers http.Header, body []byte) error {
	hooks, _ := s.doc["webhooks"].(map[string]any)
	item, ok := hooks[name].(map[string]any)
	if !ok {
		return fmt.Errorf("the document has no webhook %q", name)
	}
	op, _ := item["post"].(map[string]any)

	var problems []error
	params, _ := op["parameters"].([]any)
	for _, p := range params {
		param, err := s.resolve(p)
		if err != nil {
			return err
		}
		if param["in"] != "header" {
			continue
		}
		hname, _ := param["name"].(string)
		v := headers.Get(hname)
		if v == "" {
			if req, _ := param["required"].(bool); req {
				problems = append(problems, fmt.Errorf("required header %s is missing", hname))
			}
			continue
		}
		if schema, ok := param["schema"]; ok {
			if err := s.validate(schema, v, "header "+hname); err != nil {
				problems = append(problems, err)
			}
		}
	}

	rb, _ := op["requestBody"].(map[string]any)
	content, _ := rb["content"].(map[string]any)
	entry, _ := content["application/json"].(map[string]any)
	if err := s.validateJSON(entry["schema"], body); err != nil {
		problems = append(problems, fmt.Errorf("body: %w", err))
	}
	return errors.Join(problems...)
}

// Example returns the `example` of a component schema as JSON, so a test can
// send what the document tells readers to send.
func (s *Spec) Example(schema string) ([]byte, error) {
	schemas, _ := s.doc["components"].(map[string]any)["schemas"].(map[string]any)
	sc, ok := schemas[schema].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the document has no schema %q", schema)
	}
	ex, ok := sc["example"]
	if !ok {
		return nil, fmt.Errorf("the schema %q has no example", schema)
	}
	return json.Marshal(normalize(ex))
}

// CheckExamples validates every `example` in components/schemas against its
// own schema, so the examples in the document cannot be wrong.
func (s *Spec) CheckExamples() error {
	schemas, _ := s.doc["components"].(map[string]any)["schemas"].(map[string]any)
	var problems []error
	names := make([]string, 0, len(schemas))
	for n := range schemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		sc, _ := schemas[name].(map[string]any)
		ex, ok := sc["example"]
		if !ok {
			continue
		}
		raw, err := json.Marshal(normalize(ex))
		if err != nil {
			return err
		}
		if err := s.ValidateSchema(name, raw); err != nil {
			problems = append(problems, fmt.Errorf("example of %s: %w", name, err))
		}
	}
	return errors.Join(problems...)
}

// normalize converts yaml.v3 values (which may hold non-string keys or
// timestamps) into plain JSON-compatible values.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = normalize(x)
		}
		return out
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	}
	return v
}

func (s *Spec) validateJSON(schema any, body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("the body is not JSON: %w", err)
	}
	return s.validate(schema, v, "$")
}

// resolve follows a $ref (only local ones) to the object it names.
func (s *Spec) resolve(v any) (map[string]any, error) {
	for range 10 {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected an object, got %T", v)
		}
		ref, isRef := m["$ref"].(string)
		if !isRef {
			return m, nil
		}
		path, ok := strings.CutPrefix(ref, "#/")
		if !ok {
			return nil, fmt.Errorf("only local references are supported, got %q", ref)
		}
		var cur any = s.doc
		for _, part := range strings.Split(path, "/") {
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("reference %q does not resolve", ref)
			}
			if cur, ok = obj[part]; !ok {
				return nil, fmt.Errorf("reference %q does not resolve", ref)
			}
		}
		v = cur
	}
	return nil, errors.New("reference chain too long")
}

// supported lists the schema keywords this validator understands; annotation
// keywords have no effect on validity.
var (
	supported = map[string]bool{
		"type": true, "properties": true, "required": true, "additionalProperties": true,
		"items": true, "enum": true, "format": true, "pattern": true, "minimum": true,
		"maximum": true, "minLength": true, "maxLength": true, "minProperties": true,
	}
	annotations = map[string]bool{
		"description": true, "example": true, "examples": true, "default": true, "title": true,
		"summary": true, "deprecated": true, "$ref": true,
	}
)

func (s *Spec) validate(schemaAny any, v any, at string) error {
	schema, err := s.resolve(schemaAny)
	if err != nil {
		return fmt.Errorf("%s: %w", at, err)
	}
	for k := range schema {
		if !supported[k] && !annotations[k] {
			return fmt.Errorf("%s: the schema uses the unsupported keyword %q (extend openapicheck)", at, k)
		}
	}

	if t, ok := schema["type"]; ok {
		if err := checkType(t, v, at); err != nil {
			return err
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		match := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				match = true
			}
		}
		if !match {
			return fmt.Errorf("%s: %v is not one of %v", at, v, enum)
		}
	}

	switch val := v.(type) {
	case string:
		if f, _ := schema["format"].(string); f != "" {
			if err := checkFormat(f, val); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
		if p, ok := schema["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(val) {
			return fmt.Errorf("%s: %q does not match %s", at, val, p)
		}
		n := len([]rune(val))
		if m, ok := asFloat(schema["minLength"]); ok && float64(n) < m {
			return fmt.Errorf("%s: shorter than %v characters", at, m)
		}
		if m, ok := asFloat(schema["maxLength"]); ok && float64(n) > m {
			return fmt.Errorf("%s: longer than %v characters", at, m)
		}
	case json.Number:
		f, _ := val.Float64()
		if m, ok := asFloat(schema["minimum"]); ok && f < m {
			return fmt.Errorf("%s: %v is below the minimum %v", at, val, m)
		}
		if m, ok := asFloat(schema["maximum"]); ok && f > m {
			return fmt.Errorf("%s: %v is above the maximum %v", at, val, m)
		}
	case []any:
		if items, ok := schema["items"]; ok {
			for i, x := range val {
				if err := s.validate(items, x, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		if req, ok := schema["required"].([]any); ok {
			for _, r := range req {
				if _, present := val[r.(string)]; !present {
					return fmt.Errorf("%s: the required field %q is missing", at, r)
				}
			}
		}
		if m, ok := asFloat(schema["minProperties"]); ok && float64(len(val)) < m {
			return fmt.Errorf("%s: fewer than %v properties", at, m)
		}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sub, declared := props[k]
			if !declared {
				if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
					return fmt.Errorf("%s: the field %q is not in the document", at, k)
				}
				continue
			}
			if err := s.validate(sub, val[k], at+"."+k); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkType(t any, v any, at string) error {
	var types []string
	switch tt := t.(type) {
	case string:
		types = []string{tt}
	case []any:
		for _, x := range tt {
			types = append(types, fmt.Sprint(x))
		}
	}
	for _, want := range types {
		if matchesType(want, v) {
			return nil
		}
	}
	return fmt.Errorf("%s: expected %s, got %s", at, strings.Join(types, " or "), describe(v))
}

func matchesType(want string, v any) bool {
	switch want {
	case "null":
		return v == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		f, err := n.Float64()
		return err == nil && f == float64(int64(f))
	}
	return false
}

func describe(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", v)
}

func checkFormat(format, s string) error {
	switch format {
	case "uuid":
		if _, err := uuid.Parse(s); err != nil {
			return fmt.Errorf("%q is not a UUID", s)
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			return fmt.Errorf("%q is not an RFC 3339 date-time", s)
		}
	default:
		return fmt.Errorf("the schema uses the unsupported format %q (extend openapicheck)", format)
	}
	return nil
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
