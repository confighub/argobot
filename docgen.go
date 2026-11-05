// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/swaggest/jsonschema-go"
)

// runDocgen implements `argobot docgen env`: it writes argobot's environment
// variables as a JSON Schema, which the installer package validates its
// argobot-env ConfigMap against.
func runDocgen(args []string, out io.Writer) error {
	if len(args) != 1 || args[0] != "env" {
		return fmt.Errorf("usage: argobot docgen env")
	}
	return printEnvSchema(out)
}

// printEnvSchema writes the JSON Schema of envVars. A property it doesn't name
// is not one of argobot's variables, so it is refused rather than ignored.
func printEnvSchema(out io.Writer) error {
	reflector := jsonschema.Reflector{}
	schema, err := reflector.Reflect(envVars{})
	if err != nil {
		return fmt.Errorf("reflect the environment's schema: %w", err)
	}
	schema.AdditionalProperties = (&jsonschema.SchemaOrBool{}).WithTypeBoolean(false)
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(schema)
}
