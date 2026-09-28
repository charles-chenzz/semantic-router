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

// Package handoff defines the portable, protocol-neutral selection handoff
// contract. Transport encodings such as HTTP base64url headers belong to the
// protocol adapter, not this package.
package handoff

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

const (
	Version             = "1"
	MaxJSONBytes        = 4 * 1024
	MaxIDLength         = 128
	MaxRoleLength       = 64
	MaxCapabilityLength = 64
	MaxListItems        = 16
	MaxRemainingTokens  = 10_000_000
	MaxLifetime         = 15 * time.Minute
	maxJSONNestingDepth = 4
)

var (
	opaqueIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`)
	normalizedIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// Envelope contains only the portable facts that the Phase 1 router consumes.
// ToolStateRefs are bounded opaque identifiers; the router never dereferences
// them or copies them into model requests.
type Envelope struct {
	Version              string    `json:"version"`
	HandoffID            string    `json:"handoff_id"`
	RootInvocationID     string    `json:"root_invocation_id"`
	ParentInvocationID   string    `json:"parent_invocation_id,omitempty"`
	DelegatedRole        string    `json:"delegated_role"`
	RequiredCapabilities []string  `json:"required_capabilities,omitempty"`
	RemainingTokens      int       `json:"remaining_tokens"`
	ContextPortability   string    `json:"context_portability"`
	ToolStateRefs        []string  `json:"tool_state_refs,omitempty"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type envelopeWire struct {
	Version              *string  `json:"version"`
	HandoffID            *string  `json:"handoff_id"`
	RootInvocationID     *string  `json:"root_invocation_id"`
	ParentInvocationID   *string  `json:"parent_invocation_id,omitempty"`
	DelegatedRole        *string  `json:"delegated_role"`
	RequiredCapabilities []string `json:"required_capabilities,omitempty"`
	RemainingTokens      *int     `json:"remaining_tokens"`
	ContextPortability   *string  `json:"context_portability"`
	ToolStateRefs        []string `json:"tool_state_refs,omitempty"`
	ExpiresAt            *string  `json:"expires_at"`
}

var envelopeFieldNames = [...]string{
	"version",
	"handoff_id",
	"root_invocation_id",
	"parent_invocation_id",
	"delegated_role",
	"required_capabilities",
	"remaining_tokens",
	"context_portability",
	"tool_state_refs",
	"expires_at",
}

// ErrorCode is safe to expose as a machine-readable receipt reason. Error
// values intentionally never include envelope values.
type ErrorCode string

const (
	CodePayloadTooLarge    ErrorCode = "payload_too_large"
	CodeMalformedJSON      ErrorCode = "malformed_json"
	CodeTrailingContent    ErrorCode = "trailing_content"
	CodeDuplicateField     ErrorCode = "duplicate_field"
	CodeNestingTooDeep     ErrorCode = "nesting_too_deep"
	CodeUnknownField       ErrorCode = "unknown_field"
	CodeMissingField       ErrorCode = "missing_field"
	CodeUnsupportedVersion ErrorCode = "unsupported_version"
	CodeInvalidField       ErrorCode = "invalid_field"
	CodeTooManyItems       ErrorCode = "too_many_items"
	CodeDuplicateItem      ErrorCode = "duplicate_item"
	CodeExpired            ErrorCode = "expired"
	CodeExpiryTooFar       ErrorCode = "expiry_too_far"
)

// ContractError is a value-safe validation error.
type ContractError struct {
	Code  ErrorCode
	Field string
}

func (e *ContractError) Error() string {
	if e.Field == "" {
		return string(e.Code)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Field)
}

// CodeOf extracts a stable contract error code without exposing field values.
func CodeOf(err error) ErrorCode {
	var contractErr *ContractError
	if errors.As(err, &contractErr) {
		return contractErr.Code
	}
	return CodeMalformedJSON
}

// Parse parses compact JSON bytes and validates the portable envelope at now.
// Header decoding is deliberately outside this function.
func Parse(data []byte, now time.Time) (*Envelope, error) {
	if len(data) > MaxJSONBytes {
		return nil, contractError(CodePayloadTooLarge, "")
	}
	if err := inspectJSON(data); err != nil {
		return nil, err
	}
	wire, err := decodeEnvelopeWire(data)
	if err != nil {
		return nil, err
	}
	if err := wire.validateRequiredFields(); err != nil {
		return nil, err
	}

	expiresAt, err := parseUTCExpiry(*wire.ExpiresAt)
	if err != nil {
		return nil, err
	}
	envelope := wire.toEnvelope(expiresAt)
	if err := Validate(envelope, now); err != nil {
		return nil, err
	}
	return envelope, nil
}

func decodeEnvelopeWire(data []byte) (*envelopeWire, error) {
	var wire envelopeWire
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return nil, contractError(CodeUnknownField, "")
		}
		return nil, contractError(CodeMalformedJSON, "")
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawFields); err != nil {
		return nil, contractError(CodeMalformedJSON, "")
	}
	for _, field := range envelopeFieldNames {
		if bytes.Equal(bytes.TrimSpace(rawFields[field]), []byte("null")) {
			return nil, contractError(CodeInvalidField, field)
		}
	}
	return &wire, nil
}

func (wire *envelopeWire) validateRequiredFields() error {
	requiredFields := []struct {
		name    string
		present bool
	}{
		{name: "version", present: wire.Version != nil},
		{name: "handoff_id", present: wire.HandoffID != nil},
		{name: "root_invocation_id", present: wire.RootInvocationID != nil},
		{name: "delegated_role", present: wire.DelegatedRole != nil},
		{name: "remaining_tokens", present: wire.RemainingTokens != nil},
		{name: "context_portability", present: wire.ContextPortability != nil},
		{name: "expires_at", present: wire.ExpiresAt != nil},
	}
	for _, field := range requiredFields {
		if !field.present {
			return contractError(CodeMissingField, field.name)
		}
	}
	return nil
}

func (wire *envelopeWire) toEnvelope(expiresAt time.Time) *Envelope {
	envelope := &Envelope{
		Version:              *wire.Version,
		HandoffID:            *wire.HandoffID,
		RootInvocationID:     *wire.RootInvocationID,
		DelegatedRole:        *wire.DelegatedRole,
		RequiredCapabilities: wire.RequiredCapabilities,
		RemainingTokens:      *wire.RemainingTokens,
		ContextPortability:   *wire.ContextPortability,
		ToolStateRefs:        wire.ToolStateRefs,
		ExpiresAt:            expiresAt,
	}
	if wire.ParentInvocationID != nil {
		envelope.ParentInvocationID = *wire.ParentInvocationID
	}
	return envelope
}

// Validate validates an already decoded envelope at an explicit time.
func Validate(envelope *Envelope, now time.Time) error {
	if envelope == nil {
		return contractError(CodeMalformedJSON, "")
	}
	if envelope.Version != Version {
		return contractError(CodeUnsupportedVersion, "version")
	}
	if err := validateEnvelopeIdentity(envelope); err != nil {
		return err
	}
	if err := validateEnvelopeSelectionFields(envelope); err != nil {
		return err
	}
	return validateEnvelopeExpiry(envelope.ExpiresAt, now)
}

func validateEnvelopeIdentity(envelope *Envelope) error {
	if err := validateOpaqueID("handoff_id", envelope.HandoffID, MaxIDLength); err != nil {
		return err
	}
	if err := validateOpaqueID("root_invocation_id", envelope.RootInvocationID, MaxIDLength); err != nil {
		return err
	}
	if envelope.ParentInvocationID != "" {
		if err := validateOpaqueID("parent_invocation_id", envelope.ParentInvocationID, MaxIDLength); err != nil {
			return err
		}
	}
	return nil
}

func validateEnvelopeSelectionFields(envelope *Envelope) error {
	if err := validateNormalizedID("delegated_role", envelope.DelegatedRole, MaxRoleLength); err != nil {
		return err
	}
	if err := validateNormalizedList("required_capabilities", envelope.RequiredCapabilities, MaxCapabilityLength); err != nil {
		return err
	}
	if envelope.RemainingTokens < 0 || envelope.RemainingTokens > MaxRemainingTokens {
		return contractError(CodeInvalidField, "remaining_tokens")
	}
	if envelope.ContextPortability != "portable" && envelope.ContextPortability != "sticky" {
		return contractError(CodeInvalidField, "context_portability")
	}
	if err := validateOpaqueList("tool_state_refs", envelope.ToolStateRefs); err != nil {
		return err
	}
	return nil
}

func validateEnvelopeExpiry(expiresAt, now time.Time) error {
	if expiresAt.IsZero() {
		return contractError(CodeInvalidField, "expires_at")
	}
	if !now.Before(expiresAt) {
		return contractError(CodeExpired, "expires_at")
	}
	if expiresAt.After(now.Add(MaxLifetime)) {
		return contractError(CodeExpiryTooFar, "expires_at")
	}
	return nil
}

func inspectJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := inspectJSONValue(decoder, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); err == nil {
		return contractError(CodeTrailingContent, "")
	} else if !errors.Is(err, io.EOF) {
		return contractError(CodeMalformedJSON, "")
	}
	return nil
}

func inspectJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxJSONNestingDepth {
		return contractError(CodeNestingTooDeep, "")
	}
	token, err := decoder.Token()
	if err != nil {
		return contractError(CodeMalformedJSON, "")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	var contentsErr error
	switch delimiter {
	case '{':
		contentsErr = inspectJSONObject(decoder, depth)
	case '[':
		contentsErr = inspectJSONArray(decoder, depth)
	default:
		return contractError(CodeMalformedJSON, "")
	}
	if contentsErr != nil {
		return contentsErr
	}
	if _, err := decoder.Token(); err != nil {
		return contractError(CodeMalformedJSON, "")
	}
	return nil
}

func inspectJSONObject(decoder *json.Decoder, depth int) error {
	seen := make(map[string]struct{})
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return contractError(CodeMalformedJSON, "")
		}
		key, ok := keyToken.(string)
		if !ok {
			return contractError(CodeMalformedJSON, "")
		}
		if _, duplicate := seen[key]; duplicate {
			return contractError(CodeDuplicateField, key)
		}
		seen[key] = struct{}{}
		if err := inspectJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func inspectJSONArray(decoder *json.Decoder, depth int) error {
	for decoder.More() {
		if err := inspectJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func parseUTCExpiry(value string) (time.Time, error) {
	if !strings.HasSuffix(value, "Z") {
		return time.Time{}, contractError(CodeInvalidField, "expires_at")
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, contractError(CodeInvalidField, "expires_at")
	}
	return parsed, nil
}

func validateOpaqueID(field, value string, maxLength int) error {
	if len(value) == 0 || len(value) > maxLength || !opaqueIDPattern.MatchString(value) {
		return contractError(CodeInvalidField, field)
	}
	return nil
}

func validateNormalizedID(field, value string, maxLength int) error {
	if len(value) == 0 || len(value) > maxLength || !normalizedIDPattern.MatchString(value) {
		return contractError(CodeInvalidField, field)
	}
	return nil
}

func validateNormalizedList(field string, values []string, maxLength int) error {
	if len(values) > MaxListItems {
		return contractError(CodeTooManyItems, field)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateNormalizedID(field, value, maxLength); err != nil {
			return err
		}
		if _, duplicate := seen[value]; duplicate {
			return contractError(CodeDuplicateItem, field)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateOpaqueList(field string, values []string) error {
	if len(values) > MaxListItems {
		return contractError(CodeTooManyItems, field)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if err := validateOpaqueID(field, value, MaxIDLength); err != nil {
			return err
		}
		if _, duplicate := seen[value]; duplicate {
			return contractError(CodeDuplicateItem, field)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func contractError(code ErrorCode, field string) error {
	return &ContractError{Code: code, Field: field}
}
