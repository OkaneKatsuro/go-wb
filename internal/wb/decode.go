package wb

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// readBody читает тело ответа и распаковывает его по заголовку Content-Encoding.
// Мы шлём браузерный accept-encoding, поэтому Go не распаковывает за нас.
func readBody(resp *http.Response) ([]byte, error) {
	var r io.Reader = resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	case "deflate":
		fl := flate.NewReader(resp.Body)
		defer fl.Close()
		r = fl
	case "br":
		r = brotli.NewReader(resp.Body)
	case "zstd":
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	return io.ReadAll(r)
}
