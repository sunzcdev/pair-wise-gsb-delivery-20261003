package zapcore_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const redactionSecret = "super-secret-value"

type redactionCredentials struct {
	Password string `json:"password"`
	User     string `json:"user"`
}

type redactionStringer string

func (s redactionStringer) String() string { return string(s) }

type redactionError string

func (e redactionError) Error() string { return string(e) }

type redactionJSONMarshaler struct {
	calls int
}

type redactionCycle struct {
	Password string `json:"password"`
	Self     *redactionCycle
}

type redactionJSONCredentials struct {
	Password string `json:"password"`
}

func (redactionJSONCredentials) MarshalJSON() ([]byte, error) {
	return []byte(`{"password":"` + redactionSecret + `"}`), nil
}

type redactionJSONCredentialsPointer struct {
	Password string `json:"password"`
}

func (*redactionJSONCredentialsPointer) MarshalJSON() ([]byte, error) {
	return []byte(`{"password":"` + redactionSecret + `"}`), nil
}

func (m *redactionJSONMarshaler) MarshalJSON() ([]byte, error) {
	m.calls++
	return []byte(`{"password":"` + redactionSecret + `"}`), nil
}

func TestRedactingEncoderCredentialPassword(t *testing.T) {
	t.Parallel()

	reflected := redactionCredentials{Password: redactionSecret, User: "alice"}
	reflectedMap := map[string]interface{}{
		"password": redactionSecret,
		"user":     "alice",
	}

	fields := []zap.Field{
		zap.String("password", "ordinary-password"),
		zap.Object("credentials", zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
			enc.AddString("password", redactionSecret)
			enc.AddString("user", "alice")
			return nil
		})),
		zap.Reflect("reflectedCredentials", reflected),
		zap.Reflect("reflectedMap", reflectedMap),
	}

	output := encodeWithRedactingEncoder(t, []string{
		"credentials.password",
		"reflectedCredentials.password",
		"reflectedMap.password",
	}, fields...)
	assertRedactedOutput(t, output)

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(output), &decoded))
	assert.Equal(t, "ordinary-password", decoded["password"])

	object := decoded["credentials"].(map[string]interface{})
	assert.Equal(t, "[REDACTED]", object["password"])
	assert.Equal(t, "alice", object["user"])

	reflectedObject := decoded["reflectedCredentials"].(map[string]interface{})
	assert.Equal(t, "[REDACTED]", reflectedObject["password"])
	assert.Equal(t, "alice", reflectedObject["user"])

	mapObject := decoded["reflectedMap"].(map[string]interface{})
	assert.Equal(t, "[REDACTED]", mapObject["password"])
	assert.Equal(t, "alice", mapObject["user"])

	assert.Equal(t, redactionCredentials{Password: redactionSecret, User: "alice"}, reflected)
}

func TestRedactingEncoderNamespacesAndArrays(t *testing.T) {
	t.Parallel()

	paths := []string{"account.password", "account.items[].password", "account.credentials[].password", "account.reflectedCredentials[].password"}
	fields := []zap.Field{
		zap.String("password", "ordinary-password"),
		zap.Namespace("account"),
		zap.String("password", redactionSecret),
		zap.Array("items", zapcore.ArrayMarshalerFunc(func(arr zapcore.ArrayEncoder) error {
			return arr.AppendObject(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
				enc.AddString("password", redactionSecret)
				return nil
			}))
		})),
		zap.Array("credentials", zapcore.ArrayMarshalerFunc(func(arr zapcore.ArrayEncoder) error {
			if err := arr.AppendObject(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
				enc.AddString("password", redactionSecret)
				enc.AddString("user", "first")
				return nil
			})); err != nil {
				return err
			}
			return arr.AppendObject(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
				enc.AddString("password", redactionSecret)
				enc.AddString("user", "second")
				return nil
			}))
		})),
		zap.Reflect("reflectedCredentials", []redactionCredentials{
			{Password: redactionSecret, User: "one"},
			{Password: redactionSecret, User: "two"},
		}),
	}

	output := encodeWithRedactingEncoder(t, paths, fields...)
	assertRedactedOutput(t, output)

	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(output), &decoded))
	assert.Equal(t, "ordinary-password", decoded["password"])
	assert.Equal(t, "[REDACTED]", decoded["account"].(map[string]interface{})["password"])

	items := decoded["account"].(map[string]interface{})["items"].([]interface{})
	assert.Equal(t, "[REDACTED]", items[0].(map[string]interface{})["password"])

	credentials := decoded["account"].(map[string]interface{})["credentials"].([]interface{})
	assert.Equal(t, "[REDACTED]", credentials[0].(map[string]interface{})["password"])
	assert.Equal(t, "first", credentials[0].(map[string]interface{})["user"])
	assert.Equal(t, "[REDACTED]", credentials[1].(map[string]interface{})["password"])
	assert.Equal(t, "second", credentials[1].(map[string]interface{})["user"])

	reflectedCredentials := decoded["account"].(map[string]interface{})["reflectedCredentials"].([]interface{})
	assert.Equal(t, "[REDACTED]", reflectedCredentials[0].(map[string]interface{})["password"])
	assert.Equal(t, "one", reflectedCredentials[0].(map[string]interface{})["user"])
	assert.Equal(t, "[REDACTED]", reflectedCredentials[1].(map[string]interface{})["password"])
	assert.Equal(t, "two", reflectedCredentials[1].(map[string]interface{})["user"])
}

func TestRedactingEncoderDoesNotInvokeMarshalersOnHiddenBranches(t *testing.T) {
	t.Parallel()

	jsonMarshaler := &redactionJSONMarshaler{}
	output := encodeWithRedactingEncoder(t, nil,
		zap.Object("credentials", zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
			enc.AddString("password", redactionSecret)
			return errors.New(redactionSecret)
		})),
		zap.Namespace("credentials"),
		zap.Stringer("password", redactionStringer(redactionSecret)),
		zap.Field{Key: "password", Type: zapcore.ErrorType, Interface: redactionError(redactionSecret)},
		zap.Reflect("password", jsonMarshaler),
	)

	assert.NotContains(t, output, redactionSecret)
	assert.Contains(t, output, "[REDACTED]")
	assert.Zero(t, jsonMarshaler.calls)
}

func TestRedactingEncoderReflectArrayTerminal(t *testing.T) {
	t.Parallel()

	jsonMarshaler := &redactionJSONMarshaler{}
	output := encodeWithRedactingEncoder(t, []string{"items[]"},
		zap.Array("items", zapcore.ArrayMarshalerFunc(func(arr zapcore.ArrayEncoder) error {
			return arr.AppendReflected(jsonMarshaler)
		})),
	)

	assert.NotContains(t, output, redactionSecret)
	assert.Contains(t, output, `"items":["[REDACTED]"]`)
	assert.Zero(t, jsonMarshaler.calls)
}

func TestRedactingEncoderReflectCustomMarshalerPointer(t *testing.T) {
	t.Parallel()

	valueMarshaler := redactionJSONCredentials{Password: redactionSecret}
	pointerMarshaler := &redactionJSONCredentialsPointer{Password: redactionSecret}
	output := encodeWithRedactingEncoder(t,
		[]string{"credentials.password", "otherCredentials.password"},
		zap.Reflect("credentials", valueMarshaler),
		zap.Reflect("otherCredentials", pointerMarshaler),
	)

	assert.NotContains(t, output, redactionSecret)
	assert.Contains(t, output, "[REDACTED]")
	assert.Equal(t, redactionSecret, valueMarshaler.Password)
	assert.Equal(t, redactionSecret, pointerMarshaler.Password)
}

func TestRedactingEncoderArrayIndex(t *testing.T) {
	t.Parallel()

	output := encodeWithRedactingEncoder(t, []string{"items[1]"},
		zap.Array("items", zapcore.ArrayMarshalerFunc(func(arr zapcore.ArrayEncoder) error {
			arr.AppendString("first")
			arr.AppendString(redactionSecret)
			arr.AppendString("third")
			return nil
		})),
	)

	assert.Contains(t, output, `"items":["first","[REDACTED]","third"]`)
	assert.NotContains(t, output, redactionSecret)
}

func TestRedactingEncoderCycleDoesNotLeak(t *testing.T) {
	t.Parallel()

	cyclic := map[string]interface{}{}
	cyclic["password"] = redactionSecret
	cyclic["self"] = cyclic
	cyclicStruct := &redactionCycle{Password: redactionSecret}
	cyclicStruct.Self = cyclicStruct

	output := encodeWithRedactingEncoder(t, nil, zap.Reflect("credentials", cyclic), zap.Reflect("otherCredentials", cyclicStruct))
	assert.NotContains(t, output, redactionSecret)
	assert.Contains(t, output, "[REDACTED]")
}

func TestRedactingEncoderDisabledKeepsOriginalEncoder(t *testing.T) {
	t.Parallel()

	enc := zapcore.NewJSONEncoder(testRedactingEncoderConfig())
	assert.Same(t, zapcore.Encoder(enc), zapcore.NewRedactingEncoder(enc, zapcore.RedactingEncoderConfig{}))
}

func TestRedactingEncoderConfigurationLimits(t *testing.T) {
	t.Parallel()

	_, err := zapcore.NewRedactingEncoder(
		zapcore.NewJSONEncoder(testRedactingEncoderConfig()),
		zapcore.RedactingEncoderConfig{FieldPaths: []string{"credentials.password"}, MaxDepth: 0},
	).(zapcore.Encoder).EncodeEntry(zapcore.Entry{}, nil)
	require.Error(t, err)

	invalid := zapcore.NewRedactingEncoder(
		zapcore.NewJSONEncoder(testRedactingEncoderConfig()),
		zapcore.RedactingEncoderConfig{FieldPaths: []string{"credentials.[password"}},
	)
	_, err = invalid.EncodeEntry(zapcore.Entry{}, nil)
	require.Error(t, err)
}

func TestRedactingConsoleEncoder(t *testing.T) {
	t.Parallel()

	cfg := testRedactingEncoderConfig()
	enc := zapcore.NewRedactingEncoder(zapcore.NewConsoleEncoder(cfg), zapcore.RedactingEncoderConfig{
		FieldPaths:  []string{"credentials.password"},
		Replacement: "[REDACTED]",
		MaxDepth:    1000,
	})

	var buf bytes.Buffer
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.InfoLevel)
	require.NoError(t, core.Write(zapcore.Entry{Level: zapcore.InfoLevel, Message: "m"}, []zap.Field{
		zap.String("password", "ordinary-password"),
		zap.Object("credentials", zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
			enc.AddString("password", redactionSecret)
			return nil
		})),
	}))

	output := buf.String()
	assert.Contains(t, output, "ordinary-password")
	assert.Contains(t, output, "[REDACTED]")
	assert.NotContains(t, output, redactionSecret)
}

func encodeWithRedactingEncoder(t *testing.T, paths []string, fields ...zap.Field) string {
	t.Helper()

	if paths == nil {
		paths = []string{"credentials.password"}
	}
	enc := zapcore.NewRedactingEncoder(
		zapcore.NewJSONEncoder(testRedactingEncoderConfig()),
		zapcore.RedactingEncoderConfig{
			FieldPaths:  paths,
			Replacement: "[REDACTED]",
			MaxDepth:    1000,
		},
	)

	var buf bytes.Buffer
	core := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.InfoLevel)
	require.NoError(t, core.Write(zapcore.Entry{Level: zapcore.InfoLevel, Message: "m"}, fields))
	return strings.TrimSpace(buf.String())
}

func testRedactingEncoderConfig() zapcore.EncoderConfig {
	return zapcore.EncoderConfig{
		MessageKey:     "msg",
		LevelKey:       "level",
		TimeKey:        "",
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.EpochTimeEncoder,
		EncodeDuration: zapcore.SecondsDurationEncoder,
	}
}

func assertRedactedOutput(t *testing.T, output string) {
	t.Helper()
	assert.NotContains(t, output, redactionSecret)
	assert.Contains(t, output, "[REDACTED]")
}
