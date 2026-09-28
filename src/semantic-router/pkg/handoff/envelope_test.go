/*
Copyright 2025 vLLM Semantic Router.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handoff

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)

func TestParseCompleteEnvelope(t *testing.T) {
	data := validJSON(map[string]any{
		"parent_invocation_id":  "inv-parent",
		"required_capabilities": []string{"vision", "tools.calling"},
		"tool_state_refs":       []string{"tool-state:one", "tool-state:two"},
	})

	envelope, err := Parse(data, testNow)

	require.NoError(t, err)
	assert.Equal(t, Version, envelope.Version)
	assert.Equal(t, "handoff-1", envelope.HandoffID)
	assert.Equal(t, "inv-parent", envelope.ParentInvocationID)
	assert.Equal(t, []string{"vision", "tools.calling"}, envelope.RequiredCapabilities)
	assert.Equal(t, []string{"tool-state:one", "tool-state:two"}, envelope.ToolStateRefs)
}

func TestParseAllowsOptionalFieldsToBeAbsentOrEmpty(t *testing.T) {
	for name, overrides := range map[string]map[string]any{
		"absent": nil,
		"empty": {
			"parent_invocation_id":  "",
			"required_capabilities": []string{},
			"tool_state_refs":       []string{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			envelope, err := Parse(validJSON(overrides), testNow)
			require.NoError(t, err)
			assert.Empty(t, envelope.ParentInvocationID)
			assert.Empty(t, envelope.RequiredCapabilities)
			assert.Empty(t, envelope.ToolStateRefs)
		})
	}
}

func TestParseRejectsMalformedAndAmbiguousJSON(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		code ErrorCode
	}{
		{name: "empty", data: nil, code: CodeMalformedJSON},
		{name: "malformed", data: []byte(`{"version":`), code: CodeMalformedJSON},
		{name: "trailing", data: append(validJSON(nil), []byte(` {}`)...), code: CodeTrailingContent},
		{name: "duplicate", data: []byte(`{"version":"1","version":"1"}`), code: CodeDuplicateField},
		{name: "unknown", data: validJSON(map[string]any{"future_field": true}), code: CodeUnknownField},
		{name: "null optional list", data: validJSON(map[string]any{"tool_state_refs": nil}), code: CodeInvalidField},
		{name: "deep", data: []byte(`{"version":{"a":{"b":{"c":{"d":1}}}}}`), code: CodeNestingTooDeep},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.data, testNow)
			require.Error(t, err)
			assert.Equal(t, test.code, CodeOf(err))
		})
	}
}

func TestParseRejectsMissingRequiredFields(t *testing.T) {
	required := []string{
		"version", "handoff_id", "root_invocation_id", "delegated_role",
		"remaining_tokens", "context_portability", "expires_at",
	}
	for _, field := range required {
		t.Run(field, func(t *testing.T) {
			values := validValues()
			delete(values, field)
			data, err := json.Marshal(values)
			require.NoError(t, err)
			_, err = Parse(data, testNow)
			require.Error(t, err)
			assert.Equal(t, CodeMissingField, CodeOf(err))
			assert.Contains(t, err.Error(), field)
		})
	}
}

func TestParseRejectsVersionEnumAndTokenBounds(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]any
		code      ErrorCode
	}{
		{name: "version number", overrides: map[string]any{"version": 1}, code: CodeMalformedJSON},
		{name: "future version", overrides: map[string]any{"version": "2"}, code: CodeUnsupportedVersion},
		{name: "portability", overrides: map[string]any{"context_portability": "transfer"}, code: CodeInvalidField},
		{name: "negative tokens", overrides: map[string]any{"remaining_tokens": -1}, code: CodeInvalidField},
		{name: "too many tokens", overrides: map[string]any{"remaining_tokens": MaxRemainingTokens + 1}, code: CodeInvalidField},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(validJSON(test.overrides), testNow)
			require.Error(t, err)
			assert.Equal(t, test.code, CodeOf(err))
		})
	}
}

func TestParseAcceptsTokenBounds(t *testing.T) {
	for _, remaining := range []int{0, MaxRemainingTokens} {
		envelope, err := Parse(validJSON(map[string]any{"remaining_tokens": remaining}), testNow)
		require.NoError(t, err)
		assert.Equal(t, remaining, envelope.RemainingTokens)
	}
}

func TestParseAcceptsExactContractBounds(t *testing.T) {
	capabilities := make([]string, MaxListItems)
	toolRefs := make([]string, MaxListItems)
	for i := range capabilities {
		capabilities[i] = "cap" + string(rune('a'+i))
		toolRefs[i] = "ref" + string(rune('a'+i))
	}
	data := validJSON(map[string]any{
		"handoff_id":           strings.Repeat("a", MaxIDLength),
		"parent_invocation_id": strings.Repeat("b", MaxIDLength),
		"delegated_role":       strings.Repeat("c", MaxRoleLength),
		"required_capabilities": append(
			capabilities[:MaxListItems-1],
			strings.Repeat("d", MaxCapabilityLength),
		),
		"tool_state_refs": toolRefs,
		"expires_at":      testNow.Add(MaxLifetime).Format(time.RFC3339),
	})
	data = append(data, bytes.Repeat([]byte(" "), MaxJSONBytes-len(data))...)
	require.Len(t, data, MaxJSONBytes)

	envelope, err := Parse(data, testNow)

	require.NoError(t, err)
	assert.Len(t, envelope.RequiredCapabilities, MaxListItems)
	assert.Len(t, envelope.ToolStateRefs, MaxListItems)
}

func TestParseRejectsStringAndListBounds(t *testing.T) {
	tooMany := make([]string, MaxListItems+1)
	for i := range tooMany {
		tooMany[i] = "cap" + string(rune('a'+i))
	}
	tests := []struct {
		name      string
		overrides map[string]any
		code      ErrorCode
	}{
		{name: "empty handoff ID", overrides: map[string]any{"handoff_id": ""}, code: CodeInvalidField},
		{name: "long handoff ID", overrides: map[string]any{"handoff_id": strings.Repeat("a", MaxIDLength+1)}, code: CodeInvalidField},
		{name: "invalid root ID", overrides: map[string]any{"root_invocation_id": "spaces are forbidden"}, code: CodeInvalidField},
		{name: "long parent ID", overrides: map[string]any{"parent_invocation_id": strings.Repeat("a", MaxIDLength+1)}, code: CodeInvalidField},
		{name: "long role", overrides: map[string]any{"delegated_role": strings.Repeat("a", MaxRoleLength+1)}, code: CodeInvalidField},
		{name: "unnormalized role", overrides: map[string]any{"delegated_role": "Researcher"}, code: CodeInvalidField},
		{name: "too many capabilities", overrides: map[string]any{"required_capabilities": tooMany}, code: CodeTooManyItems},
		{name: "long capability", overrides: map[string]any{"required_capabilities": []string{strings.Repeat("a", MaxCapabilityLength+1)}}, code: CodeInvalidField},
		{name: "invalid capability", overrides: map[string]any{"required_capabilities": []string{"Vision"}}, code: CodeInvalidField},
		{name: "duplicate capability", overrides: map[string]any{"required_capabilities": []string{"vision", "vision"}}, code: CodeDuplicateItem},
		{name: "invalid tool ref", overrides: map[string]any{"tool_state_refs": []string{"tool ref"}}, code: CodeInvalidField},
		{name: "long tool ref", overrides: map[string]any{"tool_state_refs": []string{strings.Repeat("a", MaxIDLength+1)}}, code: CodeInvalidField},
		{name: "duplicate tool ref", overrides: map[string]any{"tool_state_refs": []string{"tool:1", "tool:1"}}, code: CodeDuplicateItem},
		{name: "too many tool refs", overrides: map[string]any{"tool_state_refs": tooMany}, code: CodeTooManyItems},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(validJSON(test.overrides), testNow)
			require.Error(t, err)
			assert.Equal(t, test.code, CodeOf(err))
		})
	}
}

func TestParseRejectsPayloadSize(t *testing.T) {
	_, err := Parse([]byte(strings.Repeat(" ", MaxJSONBytes+1)), testNow)
	require.Error(t, err)
	assert.Equal(t, CodePayloadTooLarge, CodeOf(err))
}

func TestParseValidatesUTCExpiryWindow(t *testing.T) {
	tests := []struct {
		name   string
		expiry string
		code   ErrorCode
	}{
		{name: "past", expiry: testNow.Add(-time.Second).Format(time.RFC3339), code: CodeExpired},
		{name: "equal now", expiry: testNow.Format(time.RFC3339), code: CodeExpired},
		{name: "too far", expiry: testNow.Add(MaxLifetime + time.Second).Format(time.RFC3339), code: CodeExpiryTooFar},
		{name: "non UTC offset", expiry: "2026-09-04T12:05:00+01:00", code: CodeInvalidField},
		{name: "invalid", expiry: "tomorrow", code: CodeInvalidField},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(validJSON(map[string]any{"expires_at": test.expiry}), testNow)
			require.Error(t, err)
			assert.Equal(t, test.code, CodeOf(err))
		})
	}
}

func TestValidateRechecksExpiryAtUseTime(t *testing.T) {
	envelope, err := Parse(validJSON(map[string]any{
		"expires_at": testNow.Add(time.Minute).Format(time.RFC3339),
	}), testNow)
	require.NoError(t, err)

	err = Validate(envelope, testNow.Add(time.Minute))
	require.Error(t, err)
	assert.Equal(t, CodeExpired, CodeOf(err))
}

func validValues() map[string]any {
	return map[string]any{
		"version":             Version,
		"handoff_id":          "handoff-1",
		"root_invocation_id":  "inv-root",
		"delegated_role":      "researcher",
		"remaining_tokens":    10_000,
		"context_portability": "portable",
		"expires_at":          testNow.Add(5 * time.Minute).Format(time.RFC3339),
	}
}

func validJSON(overrides map[string]any) []byte {
	values := validValues()
	for key, value := range overrides {
		values[key] = value
	}
	data, err := json.Marshal(values)
	if err != nil {
		panic(err)
	}
	return data
}
