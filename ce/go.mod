module adc.dev/ce

go 1.26

require (
	adc.dev/core-sdk v0.0.0
	github.com/gorilla/websocket v1.5.3
	github.com/redis/go-redis/v9 v9.22.0
	golang.org/x/crypto v0.55.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/pashagolub/pgxmock/v4 v4.5.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.10.0
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.22.0
	golang.org/x/text v0.41.0 // indirect
)

replace adc.dev/core-sdk => ../core-sdk
