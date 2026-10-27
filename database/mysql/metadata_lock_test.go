package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pulumi/golang-migrate/v4/database"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errLockWaitTimeout = &mysql.MySQLError{Number: 1205, Message: "Lock wait timeout exceeded; try restarting transaction"}

func TestParseMetadataLockTimeouts(t *testing.T) {
	testcases := []struct {
		in      string
		want    []uint
		wantErr string
	}{
		{in: "", want: nil},
		{in: "  ", want: nil},
		{in: "4", want: []uint{4}},
		{in: "1,1,4,10", want: []uint{1, 1, 4, 10}},
		{in: " 1, 4 ,10 ", want: []uint{1, 4, 10}},
		{in: "1,,4", wantErr: "not a whole number"},
		{in: "1,-4", wantErr: "not a whole number"},
		{in: "1.5", wantErr: "not a whole number"},
		{in: "1,0,4", wantErr: "at least 1 second"},
	}
	for _, tc := range testcases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseMetadataLockTimeouts(tc.in)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMetadataLockScheduleFromParams(t *testing.T) {
	testcases := []struct {
		name      string
		params    map[string]string
		wantTimes []uint
		wantDelay time.Duration
		wantErr   string
	}{
		{name: "unset", params: map[string]string{}},
		{
			name:      "schedule and delay",
			params:    map[string]string{"x-metadata-lock-timeouts": "1,1,4,10", "x-metadata-lock-retry-delay": "30s"},
			wantTimes: []uint{1, 1, 4, 10},
			wantDelay: 30 * time.Second,
		},
		{
			name:      "schedule without delay",
			params:    map[string]string{"x-metadata-lock-timeouts": "2,5"},
			wantTimes: []uint{2, 5},
		},
		{
			name:    "bad delay",
			params:  map[string]string{"x-metadata-lock-timeouts": "1", "x-metadata-lock-retry-delay": "30"},
			wantErr: "x-metadata-lock-retry-delay",
		},
		{
			name:    "bad schedule",
			params:  map[string]string{"x-metadata-lock-timeouts": "1,x"},
			wantErr: "x-metadata-lock-timeouts",
		},
		{
			name:      "legacy pair expands to a flat schedule",
			params:    map[string]string{"x-metadata-lock-timeout": "4", "x-metadata-lock-retries": "2"},
			wantTimes: []uint{4, 4, 4},
			wantDelay: time.Second / 2,
		},
		{
			name:      "legacy timeout alone makes one attempt",
			params:    map[string]string{"x-metadata-lock-timeout": "4"},
			wantTimes: []uint{4},
			wantDelay: time.Second / 2,
		},
		{
			name:      "legacy zero timeout retries under the server default",
			params:    map[string]string{"x-metadata-lock-timeout": "0", "x-metadata-lock-retries": "2"},
			wantTimes: []uint{0, 0, 0},
			wantDelay: time.Second / 2,
		},
		{
			name:      "legacy retries alone retry under the server default",
			params:    map[string]string{"x-metadata-lock-retries": "1"},
			wantTimes: []uint{0, 0},
			wantDelay: time.Second / 2,
		},
		{
			name:      "legacy zero timeout and no retries is a single attempt",
			params:    map[string]string{"x-metadata-lock-timeout": "0"},
			wantDelay: time.Second / 2,
		},
		{
			name:    "legacy retries are bounded",
			params:  map[string]string{"x-metadata-lock-timeout": "4", "x-metadata-lock-retries": "4294967295"},
			wantErr: "at most 1000",
		},
		{
			name:    "legacy and new together",
			params:  map[string]string{"x-metadata-lock-timeout": "4", "x-metadata-lock-timeouts": "1,4"},
			wantErr: "cannot be combined",
		},
	}
	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			times, delay, err := metadataLockScheduleFromParams(tc.params)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantTimes, times)
			assert.Equal(t, tc.wantDelay, delay)
		})
	}
}

func fakeRetries(timeouts []uint, delay time.Duration) (*Mysql, *[]time.Duration) {
	var slept []time.Duration
	return &Mysql{
		config: &Config{MetadataLockTimeouts: timeouts, MetadataLockRetryDelay: delay},
		sleep:  func(d time.Duration) { slept = append(slept, d) },
		jitter: func(d time.Duration) time.Duration { return d / 4 },
	}, &slept
}

func scripted(results ...error) (func(uint) error, *[]uint) {
	var seen []uint
	return func(timeout uint) error {
		seen = append(seen, timeout)
		err := results[0]
		results = results[1:]
		return err
	}, &seen
}

func TestRunWithMetadataLockRetries(t *testing.T) {
	migr := []byte("ALTER TABLE t ADD COLUMN c INT")
	lockErr := database.Error{OrigErr: errLockWaitTimeout, Err: "migration failed", Query: migr}

	t.Run("escalates until an attempt succeeds", func(t *testing.T) {
		m, slept := fakeRetries([]uint{1, 1, 4, 10}, 40*time.Second)
		attempt, seen := scripted(lockErr, lockErr, nil)

		require.NoError(t, m.runWithMetadataLockRetries(migr, attempt))
		assert.Equal(t, []uint{1, 1, 4}, *seen)
		assert.Equal(t, []time.Duration{50 * time.Second, 50 * time.Second}, *slept)
	})

	t.Run("fails once the schedule is exhausted", func(t *testing.T) {
		m, slept := fakeRetries([]uint{1, 1, 4, 10}, 40*time.Second)
		attempt, seen := scripted(lockErr, lockErr, lockErr, lockErr)

		err := m.runWithMetadataLockRetries(migr, attempt)
		assert.ErrorContains(t, err, "couldn't obtain metadata lock in 4 attempts with timeouts of [1 1 4 10] seconds")
		assert.Equal(t, []uint{1, 1, 4, 10}, *seen)
		assert.Len(t, *slept, 3, "no pause after the last attempt")
	})

	t.Run("links the help URL when the schedule is exhausted", func(t *testing.T) {
		m, _ := fakeRetries([]uint{1, 1}, 0)
		m.config.MetadataLockHelpURL = "https://example.com/doc#locks"
		attempt, _ := scripted(lockErr, lockErr)

		err := m.runWithMetadataLockRetries(migr, attempt)
		assert.ErrorContains(t, err, "timeouts of [1 1] seconds. See https://example.com/doc#locks")
	})

	t.Run("returns other errors without retrying", func(t *testing.T) {
		m, slept := fakeRetries([]uint{1, 4}, time.Second)
		syntaxErr := database.Error{OrigErr: &mysql.MySQLError{Number: 1064}, Err: "migration failed", Query: migr}
		attempt, seen := scripted(syntaxErr)

		err := m.runWithMetadataLockRetries(migr, attempt)
		assert.Equal(t, syntaxErr, err)
		assert.Equal(t, []uint{1}, *seen)
		assert.Empty(t, *slept)
	})

	t.Run("zero delay does not pause", func(t *testing.T) {
		m, slept := fakeRetries([]uint{1, 1}, 0)
		attempt, _ := scripted(lockErr, nil)

		require.NoError(t, m.runWithMetadataLockRetries(migr, attempt))
		assert.Equal(t, []time.Duration{0}, *slept)
	})
}

func TestRetryPauseJitterRange(t *testing.T) {
	m := &Mysql{config: &Config{MetadataLockRetryDelay: 30 * time.Second}}
	for range 1000 {
		pause := m.retryPause()
		require.GreaterOrEqual(t, pause, 30*time.Second)
		require.Less(t, pause, 60*time.Second)
	}
}

func TestIsLockWaitTimeout(t *testing.T) {
	assert.True(t, isLockWaitTimeout(errLockWaitTimeout))
	assert.True(t, isLockWaitTimeout(database.Error{OrigErr: errLockWaitTimeout}))
	assert.True(t, isLockWaitTimeout(&database.Error{OrigErr: errLockWaitTimeout}))
	assert.False(t, isLockWaitTimeout(nil))
	assert.False(t, isLockWaitTimeout(errors.New("1205")))
	assert.False(t, isLockWaitTimeout(&mysql.MySQLError{Number: 1213}))
}

func TestMetadataLockRetriesAgainstHeldLock(t *testing.T) {
	parallelMySQLTest(t, nil, func(t *testing.T, addr string) {
		ctx := context.Background()
		blocker, err := sql.Open("mysql", strings.TrimPrefix(addr, "mysql://"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = blocker.Close() })
		_, err = blocker.ExecContext(ctx, "CREATE TABLE held (id INT PRIMARY KEY)")
		require.NoError(t, err)

		// holdLock opens a transaction that reads `held`, taking a shared metadata lock
		// that blocks any ALTER until the transaction ends.
		holdLock := func() *sql.Tx {
			tx, err := blocker.BeginTx(ctx, nil)
			require.NoError(t, err)
			var n int
			require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM held").Scan(&n))
			return tx
		}

		d, err := (&Mysql{}).Open(addr + "?x-metadata-lock-timeouts=1,2&x-metadata-lock-retry-delay=10ms")
		require.NoError(t, err)
		t.Cleanup(func() { _ = d.Close() })
		m := d.(*Mysql)

		t.Run("statement timeout bounds each attempt, not the schedule", func(t *testing.T) {
			d, err := (&Mysql{}).Open(addr + "?x-metadata-lock-timeouts=1,1,1&x-metadata-lock-retry-delay=1s&x-statement-timeout=2500")
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			tx := holdLock()
			defer func() { _ = tx.Rollback() }()

			err = d.(*Mysql).Run(bytes.NewBufferString("ALTER TABLE held ADD COLUMN s INT"))
			assert.ErrorContains(t, err, "couldn't obtain metadata lock in 3 attempts")
		})

		t.Run("gives up after the last timeout", func(t *testing.T) {
			tx := holdLock()
			defer func() { _ = tx.Rollback() }()

			start := time.Now()
			err := m.Run(bytes.NewBufferString("ALTER TABLE held ADD COLUMN a INT"))
			assert.ErrorContains(t, err, "couldn't obtain metadata lock in 2 attempts")
			assert.GreaterOrEqual(t, time.Since(start), 3*time.Second, "waited 1s, then 2s")
		})

		t.Run("succeeds once the lock is released between attempts", func(t *testing.T) {
			tx := holdLock()
			var sleeps int
			m.sleep = func(time.Duration) {
				sleeps++
				require.NoError(t, tx.Rollback())
			}
			defer func() { m.sleep = nil }()

			require.NoError(t, m.Run(bytes.NewBufferString("ALTER TABLE held ADD COLUMN b INT")))
			assert.Equal(t, 1, sleeps)
		})
	})
}
