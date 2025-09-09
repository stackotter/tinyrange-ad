module github.com/tinyrange/ad

go 1.24.0

toolchain go1.24.2

require (
	github.com/gomarkdown/markdown v0.0.0-20250207164621-7a1f277a159e
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/mattn/go-sqlite3 v1.14.32
	github.com/tinyrange/wireguard v0.1.0
	golang.org/x/crypto v0.41.0
	golang.org/x/net v0.43.0
	gopkg.in/yaml.v3 v3.0.1
	gvisor.dev/gvisor v0.0.0-20241113022301-6fd8b69821a4
)

// replace github.com/tinyrange/wireguard v0.1.0 => github.com/stackotter/tinyrange-wireguard v0.0.0-20250910051714-355dedd0812c
replace github.com/tinyrange/wireguard v0.1.0 => ../wireguard

require (
	github.com/bazelbuild/rules_go v0.44.2 // indirect
	github.com/google/btree v1.1.2 // indirect
	golang.org/x/sys v0.36.0 // indirect
	golang.org/x/time v0.11.0 // indirect
	golang.zx2c4.com/wintun v0.0.0-20230126152724-0fa3db229ce2 // indirect
	golang.zx2c4.com/wireguard v0.0.0-20231211153847-12269c276173 // indirect
	google.golang.org/protobuf v1.33.0 // indirect
)
