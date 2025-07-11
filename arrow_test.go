//go:build duckdb_arrow

package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/stretchr/testify/require"
)

func TestArrow(t *testing.T) {
	db := openDbWrapper(t, ``)
	defer closeDbWrapper(t, db)

	createTable(t, db, `CREATE TABLE foo(bar VARCHAR, baz INTEGER)`)
	conn := openConnWrapper(t, db, context.Background())
	defer closeConnWrapper(t, conn)

	t.Run("select series", func(t *testing.T) {
		c := newConnectorWrapper(t, ``, nil)
		defer closeConnectorWrapper(t, c)

		innerConn := openDriverConnWrapper(t, c)
		defer closeDriverConnWrapper(t, &innerConn)

		ar, err := NewArrowFromConn(innerConn)
		require.NoError(t, err)

		rdr, err := ar.QueryContext(context.Background(), `SELECT * FROM generate_series(1, 10)`)
		require.NoError(t, err)
		defer rdr.Release()

		for rdr.Next() {
			rec := rdr.Record()
			require.Equal(t, int64(10), rec.NumRows())
			require.NoError(t, err)
		}
		require.NoError(t, rdr.Err())
	})

	t.Run("select long series", func(t *testing.T) {
		c := newConnectorWrapper(t, ``, nil)
		defer closeConnectorWrapper(t, c)

		innerConn := openDriverConnWrapper(t, c)
		defer closeDriverConnWrapper(t, &innerConn)

		ar, err := NewArrowFromConn(innerConn)
		require.NoError(t, err)

		rdr, err := ar.QueryContext(context.Background(), `SELECT * FROM generate_series(1, 10000)`)
		require.NoError(t, err)
		defer rdr.Release()

		var totalRows int64
		for rdr.Next() {
			rec := rdr.Record()
			totalRows += rec.NumRows()
		}
		require.Equal(t, int64(10000), totalRows)
		require.NoError(t, rdr.Err())
	})

	t.Run("query table and filter results", func(t *testing.T) {
		err := conn.Raw(func(driverConn any) error {
			innerConn, ok := driverConn.(driver.Conn)
			require.True(t, ok)

			ar, err := NewArrowFromConn(innerConn)
			require.NoError(t, err)

			_, err = db.ExecContext(context.Background(), `INSERT INTO foo (bar, baz) VALUES ('lala', 2), ('dada', 3)`)
			require.NoError(t, err)

			reader, err := ar.QueryContext(context.Background(), `SELECT bar, baz FROM foo WHERE baz > ?`, 1)
			require.NoError(t, err)
			defer reader.Release()

			for reader.Next() {
				rec := reader.Record()
				require.Equal(t, int64(2), rec.NumRows())
				require.Equal(t, "lala", rec.Column(0).ValueStr(0))
				require.Equal(t, "dada", rec.Column(0).ValueStr(1))
			}
			require.NoError(t, reader.Err())
			return nil
		})
		require.NoError(t, err)
	})

	t.Run("query error", func(t *testing.T) {
		err := conn.Raw(func(driverConn any) error {
			innerConn, ok := driverConn.(driver.Conn)
			require.True(t, ok)

			ar, err := NewArrowFromConn(innerConn)
			require.NoError(t, err)

			_, err = ar.QueryContext(context.Background(), `SELECT bar`)
			require.Error(t, err)
			return nil
		})
		require.NoError(t, err)
	})

	t.Run("register arrow stream", func(t *testing.T) {
		err := conn.Raw(func(driverConn any) error {
			innerConn, ok := driverConn.(driver.Conn)
			require.True(t, ok)

			ar, err := NewArrowFromConn(innerConn)
			require.NoError(t, err)

			pool := memory.NewGoAllocator()
			schema := arrow.NewSchema(
				[]arrow.Field{
					{Name: "f1_i32", Type: arrow.PrimitiveTypes.Int32},
					{Name: "f2_f64", Type: arrow.PrimitiveTypes.Float64},
					{Name: "f3_str", Type: arrow.BinaryTypes.String},
				},
				nil,
			)

			b := array.NewRecordBuilder(pool, schema)
			defer b.Release()

			b.Field(0).(*array.Int32Builder).AppendValues([]int32{1, 2, 3, 4, 5, 6}, nil)
			b.Field(0).(*array.Int32Builder).AppendValues([]int32{7, 8, 9, 10}, []bool{true, true, false, true})
			b.Field(1).(*array.Float64Builder).AppendValues([]float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, nil)
			b.Field(2).(*array.StringBuilder).AppendValues([]string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, nil)

			rec1 := b.NewRecord()
			defer rec1.Release()

			b.Field(0).(*array.Int32Builder).AppendValues([]int32{11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, nil)
			b.Field(1).(*array.Float64Builder).AppendValues([]float64{11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, nil)
			b.Field(2).(*array.StringBuilder).AppendValues([]string{"k", "l", "m", "n", "o", "p", "q", "r", "s", "t"}, nil)

			rec2 := b.NewRecord()
			defer rec2.Release()

			tbl := array.NewTableFromRecords(schema, []arrow.Record{rec1, rec2})
			defer tbl.Release()

			tr := array.NewTableReader(tbl, 5)
			defer tr.Release()

			ctx := context.Background()
			_, err = db.ExecContext(ctx, `CREATE TABLE conflicting (i INT, f DOUBLE, s VARCHAR)`)
			require.NoError(t, err)

			release, err := ar.RegisterView(tr, "conflicting")
			require.Nil(t, release)
			require.Error(t, err)

			release, err = ar.RegisterView(tr, "arrow_table")
			require.NoError(t, err)
			require.NotNil(t, release)
			defer release()

			_, err = db.ExecContext(ctx, `CREATE TABLE dst AS SELECT * FROM arrow_table`)
			require.NoError(t, err)

			// Query the table to verify the data.
			res, err := db.QueryContext(ctx, `SELECT * FROM dst`)
			require.NoError(t, err)
			defer closeRowsWrapper(t, res)

			i := 0
			for res.Next() {
				i++
				var f1 sql.NullInt32
				var f2 float64
				var f3 string
				require.NoError(t, res.Scan(&f1, &f2, &f3))

				if i == 9 {
					require.False(t, f1.Valid)
				} else {
					require.True(t, f1.Valid)
					require.Equal(t, int32(i), f1.Int32)
				}
				require.Equal(t, float64(i), f2)
				require.Equal(t, string(rune('a'+i-1)), f3)
			}
			require.NoError(t, res.Err())
			require.Equal(t, 20, i)
			return nil
		})
		require.NoError(t, err)
	})

	// Test concurrent reading from an Arrow stream.
	// This test creates a large Arrow stream and reads it concurrently from multiple goroutines.
	// It's not really used test case but it helps to ensure that the Arrow reader can handle concurrent access correctly.
	t.Run("concurrent arrow reader", func(t *testing.T) {
		c := newConnectorWrapper(t, ``, nil)
		defer closeConnectorWrapper(t, c)

		innerConn := openDriverConnWrapper(t, c)
		defer closeDriverConnWrapper(t, &innerConn)

		ar, err := NewArrowFromConn(innerConn)
		require.NoError(t, err)

		rdr, err := ar.QueryContext(t.Context(), `SELECT * FROM generate_series(1, 100000)`)
		require.NoError(t, err)
		defer rdr.Release()

		readCh := make(chan int64)
		wg := sync.WaitGroup{}
		mu := sync.Mutex{} // we use a mutex to synchronize Next and Record calls across goroutines
		wg.Add(10)
		for range 10 {
			go func() {
				defer wg.Done()
				rdr.Retain()
				defer rdr.Release()
				for {
					mu.Lock()
					if !rdr.Next() {
						mu.Unlock()
						return
					}
					if rdr.Err() != nil {
						t.Errorf("Error in goroutine: %v", rdr.Err())
						mu.Unlock()
						return
					}
					rec := rdr.Record()
					select {
					case <-t.Context().Done():
						mu.Unlock()
						return
					case readCh <- rec.NumRows():
					}
					mu.Unlock()
				}
			}()
		}
		var totalRows int64
		stop := make(chan struct{})
		go func() {
			defer close(stop)
			for rows := range readCh {
				totalRows += rows
			}
		}()
		wg.Wait()
		close(readCh)
		<-stop
		require.Equal(t, int64(100000), totalRows)
		require.NoError(t, rdr.Err())

		rdr.Retain()
		require.Equal(t, int64(2), rdr.(*arrowStreamReader).refCount)
		rdr.Release()
		require.Equal(t, int64(1), rdr.(*arrowStreamReader).refCount)
		rdr.Release()
		require.Equal(t, int64(0), rdr.(*arrowStreamReader).refCount)
		rdr.Release()
		require.Equal(t, int64(0), rdr.(*arrowStreamReader).refCount)
	})
}

func TestArrowClosedConn(t *testing.T) {
	db := openDbWrapper(t, ``)
	defer closeDbWrapper(t, db)

	conn := openConnWrapper(t, db, context.Background())
	err := conn.Raw(func(driverConn any) error {
		innerConn, ok := driverConn.(driver.Conn)
		require.True(t, ok)

		ar, err := NewArrowFromConn(innerConn)
		require.NoError(t, err)

		pool := memory.NewGoAllocator()
		schema := arrow.NewSchema(
			[]arrow.Field{
				{Name: "f1", Type: arrow.PrimitiveTypes.Int64},
				{Name: "f2", Type: arrow.BinaryTypes.String},
			},
			nil,
		)

		b := array.NewRecordBuilder(pool, schema)
		defer b.Release()

		b.Field(0).(*array.Int64Builder).AppendValues([]int64{1, 2, 3, 4, 5}, nil)
		b.Field(1).(*array.StringBuilder).AppendValues([]string{"a", "b", "c", "d", "e"}, nil)

		rec := b.NewRecord()
		defer rec.Release()

		tbl := array.NewTableFromRecords(schema, []arrow.Record{rec})
		defer tbl.Release()

		tr := array.NewTableReader(tbl, 5)
		defer tr.Release()

		closeDriverConnWrapper(t, &innerConn)

		release, err := ar.RegisterView(tr, "arrow_table")
		require.ErrorIs(t, err, errClosedCon)
		require.Nil(t, release)
		return driver.ErrBadConn
	})
	require.Error(t, err)
}
