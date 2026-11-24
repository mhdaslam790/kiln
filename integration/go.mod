module github.com/mhdaslam790/kiln/integration

go 1.23.2

require (
	github.com/go-sql-driver/mysql v1.9.3
	github.com/jackc/pgx/v5 v5.7.5
	github.com/mhdaslam790/kiln v1.8.0
	github.com/microsoft/go-mssqldb v1.9.2
	modernc.org/sqlite v1.38.2
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dgryski/go-rendezvous v0.0.0-20200823014737-9f7001d12a5f // indirect
	github.com/mhdaslam790/kiln/audit v0.0.0-00010101000000-000000000000 // indirect
	github.com/mhdaslam790/kiln/kilnbreaker v0.0.0-00010101000000-000000000000 // indirect
	github.com/mhdaslam790/kiln/kilncrypt v0.0.0-00010101000000-000000000000 // indirect
	github.com/mhdaslam790/kiln/kilnrw v0.0.0-00010101000000-000000000000 // indirect
	github.com/mhdaslam790/kiln/kilnshard v0.0.0-00010101000000-000000000000 // indirect
)

require (
	filippo.io/edwards25519 v1.1.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/golang-sql/civil v0.0.0-20220223132316-b832511892a9 // indirect
	github.com/golang-sql/sqlexp v0.1.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mhdaslam790/kiln/cache/rediscache v1.0.0
	github.com/ncruces/go-strftime v0.1.9 // indirect
	github.com/redis/go-redis/v9 v9.7.3
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/crypto v0.38.0 // indirect
	golang.org/x/exp v0.0.0-20250620022241-b7579e27df2b // indirect
	golang.org/x/sync v0.15.0 // indirect
	golang.org/x/sys v0.34.0 // indirect
	golang.org/x/text v0.25.0 // indirect
	modernc.org/libc v1.66.3 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

replace github.com/mhdaslam790/kiln => ../

replace github.com/mhdaslam790/kiln/audit => ../audit

replace github.com/mhdaslam790/kiln/kilncrypt => ../kilncrypt

replace github.com/mhdaslam790/kiln/kilnbreaker => ../kilnbreaker

replace github.com/mhdaslam790/kiln/kilnrw => ../kilnrw

replace github.com/mhdaslam790/kiln/kilnshard => ../kilnshard

replace github.com/mhdaslam790/kiln/cache/rediscache => ../cache/rediscache
