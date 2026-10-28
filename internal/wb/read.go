package wb

import (
	"errors"
	"fmt"
	"io"
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
