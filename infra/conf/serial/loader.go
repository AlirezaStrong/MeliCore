package serial

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"

	"github.com/BurntSushi/toml"
	"github.com/xtls/xray-core/common/errors"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf"
	json_reader "github.com/xtls/xray-core/infra/conf/json"
)

type offset struct {
	line int
	char int
}

func findOffset(b []byte, o int) *offset {
	if o >= len(b) || o < 0 {
		return nil
	}

	line := 1
	char := 0
	for i, x := range b {
		if i == o {
			break
		}
		if x == '\n' {
			line++
			char = 0
		} else {
			char++
		}
	}

	return &offset{line: line, char: char}
}

// DecodeJSONConfig reads from reader and decode the config into *conf.Config
// syntax error could be detected.
func DecodeJSONConfig(reader io.Reader) (*conf.Config, error) {
	jsonConfig := &conf.Config{}

	jsonContent := bytes.NewBuffer(make([]byte, 0, 10240))
	jsonReader := io.TeeReader(&json_reader.Reader{
		Reader: reader,
	}, jsonContent)
	decoder := json.NewDecoder(jsonReader)

	if err := decoder.Decode(jsonConfig); err != nil {
		var pos *offset
		cause := errors.Cause(err)
		switch tErr := cause.(type) {
		case *json.SyntaxError:
			pos = findOffset(jsonContent.Bytes(), int(tErr.Offset))
		case *json.UnmarshalTypeError:
			pos = findOffset(jsonContent.Bytes(), int(tErr.Offset))
		}
		if pos != nil {
			return nil, newError("failed to read config file at line ", pos.line, " char ", pos.char).Base(err)
		}
		return nil, newError("failed to read config file").Base(err)
	}

	return jsonConfig, nil
}

func LoadJSONConfig(reader io.Reader) (*core.Config, error) {
	jsonConfig, err := DecodeJSONConfig(reader)
	if err != nil {
		return nil, err
	}

	pbConfig, err := jsonConfig.Build()
	if err != nil {
		return nil, newError("failed to parse json config").Base(err)
	}

	return pbConfig, nil
}

func DecodeTOMLConfig(reader io.Reader) (*conf.Config, error) {
	tomlFile, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, newError("failed to read config file").Base(err)
	}

	configMap := make(map[string]interface{})

	if _, err := toml.Decode(string(tomlFile), &configMap); err != nil {
		return nil, newError("failed to convert TOML to Map").Base(err)
	}

	jsonFile, err := json.Marshal(&configMap)

	if err != nil {
		return nil, newError("failed to convert Map to JSON").Base(err)
	}

	return DecodeJSONConfig(bytes.NewReader(jsonFile))
}

func LoadTOMLConfig(reader io.Reader) (*core.Config, error) {
	tomlConfig, err := DecodeTOMLConfig(reader)
	if err != nil {
		return nil, err
	}

	pbConfig, err := tomlConfig.Build()
	if err != nil {
		return nil, newError("failed to parse toml config").Base(err)
	}

	return pbConfig, nil
}
