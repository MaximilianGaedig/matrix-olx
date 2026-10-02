package connector

import (
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
