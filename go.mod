module github.com/znasllc-io/memql-cockpit

go 1.26.1

require (
	github.com/gdamore/tcell/v2 v2.13.8
	github.com/gen2brain/malgo v0.11.25
	github.com/go-vgo/robotgo v1.0.2
	github.com/znasllc-io/memql v0.1.1-0.20260519030726-c52d9a687b5e
	golang.org/x/sys v0.44.0
	golang.org/x/term v0.43.0
	google.golang.org/grpc v1.79.3
	google.golang.org/protobuf v1.36.11
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/anthropics/anthropic-sdk-go v1.27.0 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dblohm7/wingoes v0.0.0-20250822163801-6d8e6105c62d // indirect
	github.com/dgraph-io/ristretto v0.2.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/ebitengine/purego v0.10.0 // indirect
	github.com/gdamore/encoding v1.0.1 // indirect
	github.com/gen2brain/shm v0.2.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jezek/xgb v1.3.0 // indirect
	github.com/jezek/xgbutil v0.0.0-20260124183602-9fd151d6a51a // indirect
	github.com/jinzhu/inflection v1.0.0 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/lib/pq v1.12.3 // indirect
	github.com/lucasb-eyer/go-colorful v1.3.0 // indirect
	github.com/lufia/plan9stats v0.0.0-20260324052639-156f7da3f749 // indirect
	github.com/otiai10/gosseract/v2 v2.4.1 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/puzpuzpuz/xsync/v3 v3.5.1 // indirect
	github.com/rivo/uniseg v0.4.7 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/santhosh-tekuri/jsonschema/v5 v5.3.1 // indirect
	github.com/sashabaranov/go-openai v1.41.2 // indirect
	github.com/shirou/gopsutil/v4 v4.26.2 // indirect
	github.com/tailscale/win v0.0.0-20250627215312-f4da2b8ee071 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/tklauser/go-sysconf v0.3.16 // indirect
	github.com/tklauser/numcpus v0.11.0 // indirect
	github.com/tmthrgd/go-hex v0.0.0-20190904060850-447a3041c3bc // indirect
	github.com/uptrace/bun v1.2.18 // indirect
	github.com/uptrace/bun/dialect/pgdialect v1.2.18 // indirect
	github.com/uptrace/bun/driver/pgdriver v1.2.16 // indirect
	github.com/vcaesar/gops v0.41.0 // indirect
	github.com/vcaesar/imgo v0.41.0 // indirect
	github.com/vcaesar/keycode v0.10.1 // indirect
	github.com/vcaesar/screenshot v0.11.1 // indirect
	github.com/vcaesar/tt v0.20.1 // indirect
	github.com/vmihailenco/msgpack/v5 v5.4.1 // indirect
	github.com/vmihailenco/tagparser/v2 v2.0.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	github.com/zeozeozeo/gomplerate v0.0.0-20250404113140-0fbb236df825 // indirect
	go.opentelemetry.io/otel v1.39.0 // indirect
	go.opentelemetry.io/otel/trace v1.39.0 // indirect
	golang.org/x/crypto v0.51.0 // indirect
	golang.org/x/exp v0.0.0-20260312153236-7ab1446f8b90 // indirect
	golang.org/x/image v0.38.0 // indirect
	golang.org/x/net v0.54.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260203192932-546029d2fa20 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	mellium.im/sasl v0.3.2 // indirect
)

// Local development: resolve memql core from the sibling tree.
// go.work alone is not sufficient because the require above is the
// epoch-zero placeholder pseudo-version, which Go can only validate
// against a real remote OR a replace pointing at a local path.
// Once memql is published as a tagged version, drop this replace
// and pin a real version in the require block above.
replace github.com/znasllc-io/memql => ../memql
