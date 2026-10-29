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
// environment against.
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
	// An AppConfig/Env file's configHub.configName and configHub.configSchema
	// lines name the file and this schema. They are ConfigHub's, not argobot's,
	// but vet-jsonschema in SDK releases through v0.9.10 validates them, as a
	// configHub object, with the variables.
	metadata := jsonschema.Schema{}
	metadata.WithType(jsonschema.Object.Type()).
		WithDescription("ConfigHub's AppConfig metadata (configHub.configName, configHub.configSchema), not one of argobot's variables.")
	schema.WithPropertiesItem("configHub", metadata.ToSchemaOrBool())
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(schema)
}
