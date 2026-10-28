module github.com/hakastein/phplslow

go 1.26.0

require (
	github.com/VKCOM/php-parser v0.8.2
	github.com/ncruces/go-sqlite3 v0.35.6
	github.com/stretchr/testify v1.12.1
)

require (
	github.com/ncruces/go-sqlite3-wasm/v6 v6.3.35304 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/sys v0.48.0 // indirect
)

replace github.com/VKCOM/php-parser => github.com/hakastein/php-parser v0.8.3-0.20261019132731-b976bc1bafe4
