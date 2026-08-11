.PHONY: build riscv64 test run

build:
	go build -o vela .

riscv64:
	GOOS=linux GOARCH=riscv64 go build -ldflags="-s -w" -o vela-riscv64 .
	@ls -lh vela-riscv64

test:
	go vet ./...
	go test ./...

run: build
	./vela

# scp the binary to the board (set NANO=root@<ip>). The deployed board keeps
# its legacy /root/nanoclaw path and S99nanoclaw supervisor; swap atomically
# and let SIGTERM drain — the supervisor respawns the new binary.
deploy: riscv64
	scp vela-riscv64 $(NANO):/root/nanoclaw.new
	@echo "on the board: mv /root/nanoclaw.new /root/nanoclaw && chmod +x /root/nanoclaw && killall nanoclaw"
