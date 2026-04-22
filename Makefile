LDFLAGS := -X github.com/fastclaw-ai/weclaw/cmd.BuildTime=$(shell date +%Y%m%d.%H%M)

dev:
	air -c .air.toml start

build:
	go build -ldflags="$(LDFLAGS)" -o weclaw .
