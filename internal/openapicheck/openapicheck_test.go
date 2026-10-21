package openapicheck

import (
	"net/http"
	"strings"
	"testing"

	"github.com/burakcelik1061-star/hook-mini/api"
)

// A validator that approves everything would make every contract test pass, so
// it is tested here against a small document, in both directions.

const mini = `
openapi: 3.1.0
info: {title: t, version: "1"}
paths:
  /things/{id}:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {$ref: "#/components/schemas/Thing"}
        "404":
          description: missing
          content:
            application/json:
              schema: {$ref: "#/components/schemas/Err"}
        "204":
          description: empty
components:
  schemas:
    Err:
      type: object
      additionalProperties: false
      required: [code]
      properties: {code: {type: string, enum: [not_found, gone]}}
    Thing:
      type: object
      additionalProperties: false
      required: [id, name, tags]
      properties:
        id: {type: string, format: uuid}
        name: {type: string, minLength: 2, maxLength: 5, pattern: "^[a-z]+$"}
        count: {type: integer, minimum: 0, maximum: 10}
        at: {type: string, format: date-time}
        next: {type: [string, "null"]}
        tags: {type: array, items: {type: string}}
        meta: {type: object}
      example: {id: "3f2b1c9e-6a5d-4c0e-9d7e-2a1b8c4d5e6f", name: abc, tags: []}
`

func TestValidatorAcceptsAndRejects(t *testing.T) {
	s, err := Load([]byte(mini))
	if err != nil {
		t.Fatal(err)
	}
	const id = `"3f2b1c9e-6a5d-4c0e-9d7e-2a1b8c4d5e6f"`
	good := `{"id":` + id + `,"name":"abc","tags":["x"],"count":3,"at":"2026-10-21T09:00:00Z","next":null,"meta":{"any":[1]}}`
	if err := s.ValidateSchema("Thing", []byte(good)); err != nil {
		t.Fatalf("a valid value was rejected: %v", err)
	}

	bad := map[string]struct{ body, want string }{
		"missing required":     {`{"id":` + id + `,"name":"abc"}`, `required field "tags"`},
		"unknown field":        {`{"id":` + id + `,"name":"abc","tags":[],"extra":1}`, `"extra" is not in the document`},
		"wrong type":           {`{"id":` + id + `,"name":5,"tags":[]}`, "expected string"},
		"bad uuid":             {`{"id":"nope","name":"abc","tags":[]}`, "not a UUID"},
		"too short":            {`{"id":` + id + `,"name":"a","tags":[]}`, "shorter"},
		"too long":             {`{"id":` + id + `,"name":"abcdef","tags":[]}`, "longer"},
		"pattern":              {`{"id":` + id + `,"name":"AB","tags":[]}`, "does not match"},
		"below minimum":        {`{"id":` + id + `,"name":"abc","tags":[],"count":-1}`, "below the minimum"},
		"above maximum":        {`{"id":` + id + `,"name":"abc","tags":[],"count":11}`, "above the maximum"},
		"not an integer":       {`{"id":` + id + `,"name":"abc","tags":[],"count":1.5}`, "expected integer"},
		"bad date":             {`{"id":` + id + `,"name":"abc","tags":[],"at":"yesterday"}`, "date-time"},
		"null where forbidden": {`{"id":` + id + `,"name":"abc","tags":null}`, "expected array, got null"},
		"wrong item type":      {`{"id":` + id + `,"name":"abc","tags":[1]}`, "$.tags[0]"},
		"not json":             {`{`, "not JSON"},
		"array for object":     {`[]`, "expected object"},
	}
	for name, c := range bad {
		err := s.ValidateSchema("Thing", []byte(c.body))
		if err == nil {
			t.Errorf("%s: an invalid value was accepted", name)
		} else if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, c.want)
		}
	}
}

func TestValidateResponseChecksStatusContentTypeAndBody(t *testing.T) {
	s, _ := Load([]byte(mini))
	ok := `{"code":"not_found"}`

	if err := s.ValidateResponse("GET", "/things/{id}", 404, "application/json; charset=utf-8", []byte(ok)); err != nil {
		t.Errorf("a valid declared response was rejected: %v", err)
	}
	if err := s.ValidateResponse("GET", "/things/{id}", 204, "", nil); err != nil {
		t.Errorf("an empty declared response was rejected: %v", err)
	}
	for name, c := range map[string]struct {
		status      int
		contentType string
		body        string
		want        string
	}{
		"undeclared status":       {418, "application/json", ok, "does not declare"},
		"body for an empty one":   {204, "application/json", ok, "without a body"},
		"wrong enum value":        {404, "application/json", `{"code":"teapot"}`, "is not one of"},
		"undeclared content type": {404, "text/plain", ok, "content type"},
		"extra field":             {404, "application/json", `{"code":"gone","x":1}`, "not in the document"},
	} {
		err := s.ValidateResponse("GET", "/things/{id}", c.status, c.contentType, []byte(c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
	if err := s.ValidateResponse("GET", "/nope", 200, "", nil); err == nil {
		t.Error("an unknown path was accepted")
	}
	if err := s.ValidateResponse("POST", "/things/{id}", 200, "", nil); err == nil {
		t.Error("an unknown method was accepted")
	}
}

func TestUnsupportedKeywordsAreRefusedNotIgnored(t *testing.T) {
	doc := strings.Replace(mini, "Err:\n      type: object", "Err:\n      oneOf: []\n      type: object", 1)
	s, err := Load([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateSchema("Err", []byte(`{"code":"gone"}`)); err == nil || !strings.Contains(err.Error(), "unsupported keyword") {
		t.Errorf("err = %v, want a refusal of the unsupported keyword", err)
	}
}

func TestLoadRejectsOtherVersions(t *testing.T) {
	if _, err := Load([]byte("openapi: 3.0.3\ninfo: {}\n")); err == nil {
		t.Error("an OpenAPI 3.0 document was accepted")
	}
	if _, err := Load([]byte(": : :")); err == nil {
		t.Error("garbage was accepted")
	}
}

func TestWebhookHeadersAndBody(t *testing.T) {
	s, err := Load(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"id":"3f2b1c9e-6a5d-4c0e-9d7e-2a1b8c4d5e6f","type":"a.b","created_at":"2026-10-21T09:00:00Z","data":{}}`
	h := http.Header{}
	h.Set("Webhook-Id", "3f2b1c9e-6a5d-4c0e-9d7e-2a1b8c4d5e6f")
	h.Set("Webhook-Timestamp", "1800000000")
	h.Set("Webhook-Signature", "v1,abc")
	if err := s.ValidateWebhook("event.delivery", h, []byte(body)); err != nil {
		t.Fatalf("a valid webhook request was rejected: %v", err)
	}

	h2 := h.Clone()
	h2.Del("Webhook-Signature")
	if err := s.ValidateWebhook("event.delivery", h2, []byte(body)); err == nil || !strings.Contains(err.Error(), "webhook-signature") {
		t.Errorf("a missing required header was accepted: %v", err)
	}
	h3 := h.Clone()
	h3.Set("Webhook-Timestamp", "yesterday")
	if err := s.ValidateWebhook("event.delivery", h3, []byte(body)); err == nil {
		t.Error("a malformed timestamp header was accepted")
	}
	if err := s.ValidateWebhook("event.delivery", h, []byte(`{"id":"x"}`)); err == nil {
		t.Error("an invalid body was accepted")
	}
}

// The document's own examples must be valid against its own schemas.
func TestTheDocumentsExamplesAreValid(t *testing.T) {
	s, err := Load(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckExamples(); err != nil {
		t.Fatal(err)
	}
}

func TestEveryOperationHasAnOperationIDAndStatuses(t *testing.T) {
	s, err := Load(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	ops := s.Operations()
	if len(ops) < 15 {
		t.Fatalf("only %d operations found", len(ops))
	}
	seen := map[string]bool{}
	for _, op := range ops {
		st, err := s.Statuses(op.Method, op.Path)
		if err != nil || len(st) == 0 {
			t.Errorf("%s declares no responses (%v)", op, err)
		}
		o, _ := s.operation(op.Method, op.Path)
		id, _ := o["operationId"].(string)
		if id == "" || seen[id] {
			t.Errorf("%s has a missing or duplicate operationId %q", op, id)
		}
		seen[id] = true
	}
}
