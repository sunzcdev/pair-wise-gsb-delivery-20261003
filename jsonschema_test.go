package toml

import (
	"bytes"
	"strings"
	"testing"
)

const jsonSchemaSampleOne = `
name = "first"

[servers."alpha.example"]
host = "10.0.0.1"
port = 8080
tags = ["primary"]
limits = []
maybe = "v1"
when_date = 2026-01-02
when_time = 03:04:05
when_local = 2026-01-02T03:04:05
when_offset = 2026-01-02T03:04:05Z

[servers."beta.example"]
host = "10.0.0.2"
port = 8081
tags = ["cache"]
limits = []
maybe = "v2"
when_date = 2026-01-03
when_time = 04:05:06
when_local = 2026-01-03T04:05:06
when_offset = 2026-01-03T04:05:06+08:00
`

const jsonSchemaSampleTwo = `
title = "second"

[servers."alpha.example"]
port = 9090
tags = ["replica", 2]
limits = []

[servers."beta.example"]
host = "10.0.0.3"
port = 9091
tags = [3, { enabled = true }]

[[jobs]]
name = "backup"
retries = 2

[[jobs]]
name = "report"
ratio = 1.5
`

const expectedJSONSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "jobs": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {
            "type": "string"
          },
          "ratio": {
            "type": "number"
          },
          "retries": {
            "type": "integer"
          }
        },
        "required": [
          "name"
        ]
      }
    },
    "name": {
      "type": "string"
    },
    "servers": {
      "type": "object",
      "properties": {
        "alpha.example": {
          "type": "object",
          "properties": {
            "host": {
              "type": "string"
            },
            "limits": {
              "type": "array"
            },
            "maybe": {
              "type": "string"
            },
            "port": {
              "type": "integer"
            },
            "tags": {
              "type": "array",
              "items": {
                "anyOf": [
                  {
                    "type": "integer"
                  },
                  {
                    "type": "string"
                  }
                ]
              }
            },
            "when_date": {
              "type": "string",
              "x-toml-type": "local-date"
            },
            "when_local": {
              "type": "string",
              "x-toml-type": "local-date-time"
            },
            "when_offset": {
              "type": "string",
              "x-toml-type": "offset-date-time"
            },
            "when_time": {
              "type": "string",
              "x-toml-type": "local-time"
            }
          },
          "required": [
            "limits",
            "port",
            "tags"
          ]
        },
        "beta.example": {
          "type": "object",
          "properties": {
            "host": {
              "type": "string"
            },
            "limits": {
              "type": "array"
            },
            "maybe": {
              "type": "string"
            },
            "port": {
              "type": "integer"
            },
            "tags": {
              "type": "array",
              "items": {
                "anyOf": [
                  {
                    "type": "integer"
                  },
                  {
                    "type": "object",
                    "properties": {
                      "enabled": {
                        "type": "boolean"
                      }
                    },
                    "required": [
                      "enabled"
                    ]
                  },
                  {
                    "type": "string"
                  }
                ]
              }
            },
            "when_date": {
              "type": "string",
              "x-toml-type": "local-date"
            },
            "when_local": {
              "type": "string",
              "x-toml-type": "local-date-time"
            },
            "when_offset": {
              "type": "string",
              "x-toml-type": "offset-date-time"
            },
            "when_time": {
              "type": "string",
              "x-toml-type": "local-time"
            }
          },
          "required": [
            "host",
            "port",
            "tags"
          ]
        }
      },
      "required": [
        "alpha.example",
        "beta.example"
      ]
    },
    "title": {
      "type": "string"
    }
  },
  "required": [
    "servers"
  ]
}`

func TestInferJSONSchema(t *testing.T) {
	schema, err := InferJSONSchema([]byte(jsonSchemaSampleOne), []byte(jsonSchemaSampleTwo))
	if err != nil {
		t.Fatalf("InferJSONSchema returned error: %v", err)
	}

	if got := string(schema); got != expectedJSONSchema {
		t.Fatalf("unexpected schema:\n%s", got)
	}
}

func TestInferJSONSchemaSampleOrder(t *testing.T) {
	first, err := InferJSONSchema([]byte(jsonSchemaSampleOne), []byte(jsonSchemaSampleTwo))
	if err != nil {
		t.Fatalf("InferJSONSchema returned error: %v", err)
	}
	second, err := InferJSONSchema([]byte(jsonSchemaSampleTwo), []byte(jsonSchemaSampleOne))
	if err != nil {
		t.Fatalf("InferJSONSchema with reversed samples returned error: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("schema depends on sample order:\n%s\n%s", first, second)
	}
}

func TestInferJSONSchemaEmptyArrayStaysUnknown(t *testing.T) {
	schema, err := InferJSONSchema(
		[]byte("items = []\n"),
		[]byte("items = [1]\n"),
	)
	if err != nil {
		t.Fatalf("InferJSONSchema returned error: %v", err)
	}

	const expected = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "items": {
      "type": "array",
      "items": {
        "type": "integer"
      }
    }
  },
  "required": [
    "items"
  ]
}`
	if got := string(schema); got != expected {
		t.Fatalf("unexpected schema:\n%s", got)
	}
}

func TestInferJSONSchemaHeterogeneousProperties(t *testing.T) {
	schema, err := InferJSONSchema(
		[]byte("mode = true\n[meta]\nvalue = { enabled = false }\n"),
		[]byte("mode = 3\n[meta]\nvalue = 2.5\n"),
	)
	if err != nil {
		t.Fatalf("InferJSONSchema returned error: %v", err)
	}

	const expected = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "meta": {
      "type": "object",
      "properties": {
        "value": {
          "anyOf": [
            {
              "type": "number"
            },
            {
              "type": "object",
              "properties": {
                "enabled": {
                  "type": "boolean"
                }
              },
              "required": [
                "enabled"
              ]
            }
          ]
        }
      },
      "required": [
        "value"
      ]
    },
    "mode": {
      "anyOf": [
        {
          "type": "boolean"
        },
        {
          "type": "integer"
        }
      ]
    }
  },
  "required": [
    "meta",
    "mode"
  ]
}`
	if got := string(schema); got != expected {
		t.Fatalf("unexpected schema:\n%s", got)
	}
}

func TestInferJSONSchemaErrors(t *testing.T) {
	if _, err := InferJSONSchema(); err == nil {
		t.Fatal("expected an error for no samples")
	}

	_, err := InferJSONSchema([]byte("name ="))
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "TOML sample 1") {
		t.Fatalf("error does not identify sample: %v", err)
	}

	if _, err := inferJSONSchemaValue(struct{}{}); err == nil {
		t.Fatal("expected an error for an unsupported value type")
	}
}
