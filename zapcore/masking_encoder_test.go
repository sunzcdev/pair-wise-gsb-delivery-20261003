package zapcore

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type maskingCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type maskingAccount struct {
	Password    string             `json:"accountPassword"`
	Credentials maskingCredentials `json:"credentials"`
}

type maskingAccountList struct {
	Accounts []maskingCredentials `json:"accounts"`
}

type maskingCredentialObject struct {
	password string
}

func (o maskingCredentialObject) MarshalLogObject(enc ObjectEncoder) error {
	enc.AddString("username", "alice")
	enc.AddString("password", o.password)
	return nil
}

type maskingCredentialArray []string

func (a maskingCredentialArray) MarshalLogArray(enc ArrayEncoder) error {
	enc.AppendString("alice")
	enc.AppendString(a[1])
	return nil
}

type maskingNestedArray struct {
	items maskingCredentialArray
}

func (o maskingNestedArray) MarshalLogObject(enc ObjectEncoder) error {
	return enc.AddArray("items", o.items)
}

type maskingObjectError struct {
	password string
}

func (o maskingObjectError) MarshalLogObject(ObjectEncoder) error {
	return errors.New(o.password)
}

type maskingArrayError []string

func (a maskingArrayError) MarshalLogArray(ArrayEncoder) error {
	return errors.New(a[0])
}

type maskingBypassObject struct{}

func (maskingBypassObject) MarshalLogObject(enc ObjectEncoder) error {
	if concrete, ok := enc.(interface{ AddString(string, string) }); ok {
		concrete.AddString("password", "bypass-secret")
	}
	return nil
}

type maskingJSONMarshaler struct {
	password string
	called   *bool
}

func (m maskingJSONMarshaler) MarshalJSON() ([]byte, error) {
	*m.called = true
	return json.Marshal(struct {
		Password string `json:"password"`
	}{Password: m.password})
}

func newMaskingTestEncoder(t *testing.T, paths ...string) Encoder {
	t.Helper()

	enc, err := NewMaskingEncoder(NewJSONEncoder(EncoderConfig{}), "***", paths...)
	require.NoError(t, err)
	return enc
}

func encodeMaskingFields(t *testing.T, enc Encoder, fields ...Field) map[string]interface{} {
	t.Helper()

	buf, err := enc.EncodeEntry(Entry{}, fields)
	require.NoError(t, err)
	defer buf.Free()

	var output map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
	return output
}

func TestMaskingEncoderNoPathsReturnsOriginal(t *testing.T) {
	base := NewJSONEncoder(EncoderConfig{})

	enc, err := NewMaskingEncoder(base, "***")

	require.NoError(t, err)
	assert.Same(t, base, enc)
}

func TestMaskingEncoderFullPath(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password")

	output := encodeMaskingFields(t, enc,
		Field{Key: "password", Type: StringType, String: "keep-password"},
		Field{Key: "credentials.password", Type: StringType, String: "keep-raw-key"},
		Field{Key: "credentials", Type: ObjectMarshalerType, Interface: maskingCredentialObject{password: "nested-secret"}},
	)

	assert.Equal(t, "keep-password", output["password"])
	assert.Equal(t, "keep-raw-key", output["credentials.password"])
	credentials := output["credentials"].(map[string]interface{})
	assert.Equal(t, "alice", credentials["username"])
	assert.Equal(t, "***", credentials["password"])
}

func TestMaskingEncoderNamespace(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password")

	output := encodeMaskingFields(t, enc,
		Field{Key: "credentials", Type: NamespaceType},
		Field{Key: "password", Type: StringType, String: "namespace-secret"},
		Field{Key: "username", Type: StringType, String: "bob"},
	)

	credentials := output["credentials"].(map[string]interface{})
	assert.Equal(t, "***", credentials["password"])
	assert.Equal(t, "bob", credentials["username"])
}

func TestMaskingEncoderInlineMarshaler(t *testing.T) {
	enc := newMaskingTestEncoder(t, "password")

	output := encodeMaskingFields(t, enc,
		Field{
			Type:      InlineMarshalerType,
			Interface: maskingCredentialObject{password: "inline-secret"},
		},
	)

	assert.Equal(t, "alice", output["username"])
	assert.Equal(t, "***", output["password"])
}

func TestMaskingEncoderArrayAndReflection(t *testing.T) {
	enc := newMaskingTestEncoder(t,
		"credentials.password",
		"credentialMap.password",
		"credentialsArray[]",
		"accounts[].password",
		"transparentAccounts.password",
		"credentials.credentials.password",
	)

	structValue := maskingAccount{
		Password: "account-password",
		Credentials: maskingCredentials{
			Username: "alice",
			Password: "struct-secret",
		},
	}
	mapValue := map[string]interface{}{
		"username": "mallory",
		"password": "map-secret",
	}
	accounts := []maskingCredentials{{
		Username: "one",
		Password: "one-secret",
	}, {
		Username: "two",
		Password: "two-secret",
	}}
	transparentAccounts := []maskingCredentials{{
		Username: "three",
		Password: "transparent-secret",
	}}

	output := encodeMaskingFields(t, enc,
		Field{Key: "credentials", Type: ReflectType, Interface: structValue},
		Field{Key: "credentialMap", Type: ReflectType, Interface: mapValue},
		Field{Key: "accounts", Type: ReflectType, Interface: accounts},
		Field{Key: "transparentAccounts", Type: ReflectType, Interface: transparentAccounts},
		Field{Key: "credentialsArray", Type: ArrayMarshalerType, Interface: maskingCredentialArray{"", "array-secret"}},
	)

	credentials := output["credentials"].(map[string]interface{})
	assert.Equal(t, "account-password", credentials["accountPassword"])
	nested := credentials["credentials"].(map[string]interface{})
	assert.Equal(t, "alice", nested["username"])
	assert.Equal(t, "***", nested["password"])

	credentialMap := output["credentialMap"].(map[string]interface{})
	assert.Equal(t, "mallory", credentialMap["username"])
	assert.Equal(t, "***", credentialMap["password"])

	encodedAccounts := output["accounts"].([]interface{})
	for _, item := range encodedAccounts {
		account := item.(map[string]interface{})
		assert.Equal(t, "***", account["password"])
		assert.Contains(t, []string{"one", "two"}, account["username"])
	}
	transparent := output["transparentAccounts"].([]interface{})
	assert.Equal(t, "***", transparent[0].(map[string]interface{})["password"])
	assert.Equal(t, []interface{}{"***", "***"}, output["credentialsArray"])
}

func TestMaskingEncoderCustomMarshalerErrorsDoNotLeak(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password", "secrets[]")

	buf, err := enc.EncodeEntry(Entry{}, []Field{
		Field{Key: "credentials", Type: ObjectMarshalerType, Interface: maskingObjectError{password: "object-error-secret"}},
		Field{Key: "secrets", Type: ArrayMarshalerType, Interface: maskingArrayError{"array-error-secret"}},
	})

	require.NoError(t, err)
	defer buf.Free()
	assert.NotContains(t, string(buf.Bytes()), "object-error-secret")
	assert.NotContains(t, string(buf.Bytes()), "array-error-secret")

	var output map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
	assert.Equal(t, "value could not be redacted", output["credentialsError"])
	assert.Equal(t, "value could not be redacted", output["secretsError"])
}

func TestMaskingEncoderBypassAttemptStillMasked(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password")

	output := encodeMaskingFields(t, enc,
		Field{Key: "credentials", Type: ObjectMarshalerType, Interface: maskingBypassObject{}},
	)

	credentials := output["credentials"].(map[string]interface{})
	assert.Equal(t, "***", credentials["password"])
}

func TestMaskingEncoderCustomJSONMarshalerIsNotInvokedInsideRule(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password")
	called := false

	output := encodeMaskingFields(t, enc,
		Field{Key: "credentials", Type: ReflectType, Interface: maskingJSONMarshaler{password: "json-secret", called: &called}},
	)

	assert.False(t, called)
	assert.Equal(t, "***", output["credentials"])

	buf, err := enc.EncodeEntry(Entry{}, []Field{
		Field{Key: "other", Type: ReflectType, Interface: maskingJSONMarshaler{password: "passthrough-secret", called: &called}},
	})
	require.NoError(t, err)
	defer buf.Free()
	assert.Contains(t, string(buf.Bytes()), "passthrough-secret")
}

func TestMaskingEncoderStringerAndErrorAreNotEvaluatedAtTarget(t *testing.T) {
	calls := 0
	stringer := stringerFunc(func() string {
		calls++
		return "stringer-secret"
	})
	err := errorFunc(func() string {
		calls++
		return "error-secret"
	})
	enc := newMaskingTestEncoder(t, "credentials.password")

	output := encodeMaskingFields(t, enc,
		Field{Key: "credentials", Type: NamespaceType},
		Field{Key: "password", Type: StringerType, Interface: stringer},
		Field{Key: "otherError", Type: ErrorType, Interface: err},
	)

	credentials := output["credentials"].(map[string]interface{})
	assert.Equal(t, "***", credentials["password"])
	assert.Equal(t, "error-secret", credentials["otherError"])
	assert.Equal(t, 1, calls)
}

func TestMaskingEncoderDoesNotMutateInput(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password")
	input := maskingAccount{
		Password: "top-level-password",
		Credentials: maskingCredentials{
			Username: "alice",
			Password: "nested-secret",
		},
	}

	_ = encodeMaskingFields(t, enc, Field{Key: "credentials", Type: ReflectType, Interface: input})

	assert.Equal(t, "top-level-password", input.Password)
	assert.Equal(t, "nested-secret", input.Credentials.Password)
}

func TestMaskingEncoderNestedReflectedArrays(t *testing.T) {
	enc := newMaskingTestEncoder(t, "root.accounts[][].password")
	value := map[string]interface{}{
		"accounts": [][]map[string]string{{{
			"username": "alice",
			"password": "nested-array-secret",
		}}},
	}

	output := encodeMaskingFields(t, enc,
		Field{Key: "root", Type: ReflectType, Interface: value},
	)

	root := output["root"].(map[string]interface{})
	accounts := root["accounts"].([]interface{})
	first := accounts[0].([]interface{})
	account := first[0].(map[string]interface{})
	assert.Equal(t, "alice", account["username"])
	assert.Equal(t, "***", account["password"])
}

func TestMaskingEncoderCustomNestedArray(t *testing.T) {
	enc := newMaskingTestEncoder(t, "root.items[]")

	output := encodeMaskingFields(t, enc,
		Field{
			Key:       "root",
			Type:      ObjectMarshalerType,
			Interface: maskingNestedArray{items: maskingCredentialArray{"", "nested-array-secret"}},
		},
	)

	root := output["root"].(map[string]interface{})
	assert.Equal(t, []interface{}{"***", "***"}, root["items"])
}

func TestMaskingEncoderTypedNilReflection(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password")

	output := encodeMaskingFields(t, enc,
		Field{Key: "credentials", Type: ReflectType, Interface: (*maskingCredentials)(nil)},
	)

	assert.Nil(t, output["credentials"])
}

func TestMaskingEncoderReflectCycleDoesNotContainValue(t *testing.T) {
	enc := newMaskingTestEncoder(t, "credentials.password")
	type cycle struct {
		Password string `json:"password"`
		Self     *cycle `json:"self"`
	}
	value := &cycle{Password: "cycle-secret"}
	value.Self = value

	buf, err := enc.EncodeEntry(Entry{}, []Field{
		{Key: "credentials", Type: ReflectType, Interface: value},
	})

	require.NoError(t, err)
	defer buf.Free()
	assert.NotContains(t, string(buf.Bytes()), "cycle-secret")
}

func TestMaskingEncoderRejectsInvalidPaths(t *testing.T) {
	for _, path := range []string{"", ".credentials", "credentials.", "credentials.[", "credentials[0]"} {
		_, err := NewMaskingEncoder(NewJSONEncoder(EncoderConfig{}), "***", path)
		assert.Error(t, err, path)
	}
}

type stringerFunc func() string

func (f stringerFunc) String() string {
	return f()
}

type errorFunc func() string

func (f errorFunc) Error() string {
	return f()
}
