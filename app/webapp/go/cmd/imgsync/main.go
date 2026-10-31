// imgsync は初期データの画像 (不変) を DB からローカルディスクへ書き出す。
// アプリは画像の閲覧可否を毎回 DB で確認したうえで、本体をこのディスクから読む。
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"example.com/isureal-company/isureal"
	"github.com/jmoiron/sqlx"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	config := isureal.ConfigFromEnv()
	db, err := sqlx.Open("pgx", config.DatabaseDSN())
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	// reset.sql が TRUNCATE する境界より後のパーティションは読まない
	// (読み取り中に initialize が来ると TRUNCATE がロック待ちになる。初期データの画像も入っていない)。
	// 引数でパーティション名を渡すと、そのうち対象のものだけを読む (途中で止めた分の再開用)。
	var partitions []string
	if err := db.SelectContext(ctx, &partitions, `
  SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
  WHERE i.inhparent = 'image_objects'::regclass
    AND c.relname <= 'image_objects_' || to_char((SELECT created_at FROM initial_data_boundary) AT TIME ZONE 'UTC', 'YYYYMM')
    AND (coalesce(cardinality($1::text[]), 0) = 0 OR c.relname = ANY($1::text[]))
  ORDER BY c.relname`, os.Args[1:]); err != nil {
		panic(err)
	}
	work := make(chan string)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for partition := range work {
				n, err := syncPartition(ctx, db, partition)
				fmt.Println(partition, n, err)
			}
		}()
	}
	for _, p := range partitions {
		work <- p
	}
	close(work)
	wg.Wait()
}

func syncPartition(ctx context.Context, db *sqlx.DB, partition string) (int, error) {
	rows, err := db.QueryxContext(ctx, `SELECT id, data FROM `+partition+` WHERE created_at <= (SELECT created_at FROM initial_data_boundary)`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return n, err
		}
		if err := isureal.WriteImageToDisk(id, data); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}
