// Пакет wb общается с Wildberries: поисковый API (через site-mimic)
// и публичный CDN с юридическими данными продавцов.
package wb

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// SellerHit — один продавец, найденный источником Source. Обязателен только SupplierID.
type SellerHit struct {
	SupplierID     int
	Supplier       string
	SupplierRating float64
	Query          string // запрос, которым нашли продавца; пусто для списков id
}

// Source выдаёт найденных продавцов.
//
// Реализации в этом пакете: IDList (готовые id), QuerySearch (поиск WB по
// запросам), Multi (несколько источников подряд). Подойдёт что угодно, что умеет
// вызывать emit, поэтому новые способы находить продавцов подключаются сюда.
//
// Контракт: emit вызывается синхронно из той же горутины, что вызвала Collect,
// а Collect возвращает ctx.Err(), как только ctx отменён.
type Source interface {
	Collect(ctx context.Context, emit func(SellerHit)) error
}

// IDList — это Source поверх готового списка supplierId.
type IDList []int

func (l IDList) Collect(ctx context.Context, emit func(SellerHit)) error {
	for _, id := range l {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id > 0 {
			emit(SellerHit{SupplierID: id})
		}
	}
	return nil
}

// Multi запускает несколько источников по очереди и останавливается на первой ошибке.
type Multi []Source

func (m Multi) Collect(ctx context.Context, emit func(SellerHit)) error {
	for _, s := range m {
		if err := s.Collect(ctx, emit); err != nil {
			return err
		}
	}
	return nil
}

// QuerySearch прогоняет Searcher по нескольким запросам.
//
// Упавший запрос логируется и пропускается. ErrChallenge останавливает весь
// прогон, потому что каждый следующий запрос получил бы тот же ответ.
type QuerySearch struct {
	Searcher *Searcher
	Queries  []string
	Pages    int
}

func (q QuerySearch) Collect(ctx context.Context, emit func(SellerHit)) error {
	for i, query := range q.Queries {
		slog.Info("поиск", "n", i+1, "of", len(q.Queries), "query", query)
		err := q.Searcher.Collect(ctx, query, q.Pages, emit)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, ErrChallenge) {
			return err
		}
		if err != nil {
			slog.Warn("запрос пропущен", "query", query, "err", err)
		}
	}
	return nil
}

// sleep ждёт d или отмены ctx — что наступит раньше.
// time.Sleep прервать нельзя, а это можно.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
