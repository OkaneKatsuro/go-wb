package wb

import (
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// ErrBodyTooLarge — тело ответа превысило лимит ReadLimited.
var ErrBodyTooLarge = errors.New("тело ответа больше допустимого")

// ReadLimited читает r целиком, но не больше limit байт; при превышении
// возвращает ErrBodyTooLarge. Общий предохранитель для всех HTTP-клиентов проекта.
func ReadLimited(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w (лимит %d байт)", ErrBodyTooLarge, limit)
	}
	return b, nil
}

// readBody читает тело ответа (не больше limit байт) и распаковывает его по
// заголовку Content-Encoding. Мы шлём браузерный accept-encoding, поэтому Go не
// распаковывает за нас. Лимит считается после распаковки: это защита и от
// гигантского ответа, и от «бомбы» из сжатых нулей.
func readBody(resp *http.Response, limit int64) ([]byte, error) {
	var r io.Reader = resp.Body
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))) {
	case "gzip":
		// Close у gzip.Reader и flate не освобождает ресурсов и не закрывает
		// resp.Body, поэтому его не вызываем; resp.Body закрывает вызывающий.
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		r = gz
	case "deflate":
		r = flate.NewReader(resp.Body)
	case "br":
		r = brotli.NewReader(resp.Body)
	case "zstd":
		// zstd запускает горутины-декодеры, Close их останавливает.
		zr, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	return ReadLimited(r, limit)
}
