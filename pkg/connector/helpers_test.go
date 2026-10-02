package connector

import (
	"fmt"

	up "go.mau.fi/util/configupgrade"
	"image"
	"image/gif"
	"io"

	"gopkg.in/yaml.v3"
)

func yamlUnmarshal(data string, into any) error {
	return yaml.Unmarshal([]byte(data), into)
}

func encodeGIF(w io.Writer, img image.Image) error {
	return gif.Encode(w, img, nil)
}

// upgradeConfig runs the connector's config upgrader on a network config the
// way the bridge does at startup, turning a panic into an error.
func runConfigUpgrade(upgrader up.Upgrader, cfg string) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("upgrader panicked: %v", r)
		}
	}()
	data, err := up.DoBytes([]byte(cfg), upgrader.(up.BaseUpgrader))
	return string(data), err
}
