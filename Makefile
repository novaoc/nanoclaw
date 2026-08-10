.PHONY: build riscv64 test run

build:
	go build -o nanoclaw .

riscv64:
	GOOS=linux GOARCH=riscv64 go build -ldflags="-s -w" -o nanoclaw-riscv64 .
	@ls -lh nanoclaw-riscv64

test:
	go vet ./...
	go test ./...

run: build
	./nanoclaw

# scp the binary + config to the board (set NANO=root@<ip>)
deploy: riscv64
	scp nanoclaw-riscv64 $(NANO):/root/nanoclaw
	@echo "now on the board: /etc/init.d/S99nanoclaw restart  (or systemctl restart nanoclaw)"
