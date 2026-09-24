package product

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AstrolabeGTM/astrolabe/internal/store"
)

// Sync writes loaded products and their sequences into the database so jobs
// and queries see the same configuration as the files.
func Sync(ctx context.Context, pool *pgxpool.Pool, products []*Product) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, p := range products {
			cfg, err := json.Marshal(p)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO products (id, name, config, file_hash) VALUES ($1, $2, $3, $4)
				ON CONFLICT (id) DO UPDATE SET name = $2, config = $3, file_hash = $4, loaded_at = now()
				WHERE products.file_hash <> $4`, p.ID, p.Name, cfg, p.Hash); err != nil {
				return err
			}
			for id, s := range p.Sequences {
				cfg, err := json.Marshal(s)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `
					INSERT INTO sequences (product_id, id, config) VALUES ($1, $2, $3)
					ON CONFLICT (product_id, id) DO UPDATE SET config = $3`, p.ID, id, cfg); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Get reads a product's configuration as last synced.
func Get(ctx context.Context, q store.Q, id string) (*Product, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT config FROM products WHERE id = $1`, id).Scan(&raw); err != nil {
		return nil, err
	}
	p := &Product{}
	return p, json.Unmarshal(raw, p)
}

// GetSequence reads one sequence as last synced.
func GetSequence(ctx context.Context, q store.Q, productID, id string) (*Sequence, error) {
	var raw []byte
	if err := q.QueryRow(ctx, `SELECT config FROM sequences WHERE product_id = $1 AND id = $2`, productID, id).Scan(&raw); err != nil {
		return nil, err
	}
	s := &Sequence{}
	return s, json.Unmarshal(raw, s)
}

// All returns every synced product.
func All(ctx context.Context, q store.Q) ([]*Product, error) {
	rows, err := q.Query(ctx, `SELECT config FROM products ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Product
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		p := &Product{}
		if err := json.Unmarshal(raw, p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
