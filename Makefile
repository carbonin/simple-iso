IMAGE := $(or ${IMAGE}, quay.io/carbonin/config-image-server:latest)
PWD = $(shell pwd)
PORT := $(or ${PORT}, 8080)

build:
	podman build -f Dockerfile . -t $(IMAGE)

push: build
	podman push $(IMAGE)

lint:
	golangci-lint run -v

test:
	go test

generate:
	go generate $(shell go list ./...)
	$(MAKE) format

format:
	@goimports -w -l main.go internal pkg || /bin/true

run: certs
	podman run --rm \
		-v $(PWD)/data:/data:Z \
		-v $(PWD)/certs:/certs:Z \
		-p$(PORT):$(PORT) \
		-e PORT=$(PORT) \
		-e DATA_DIR=/data \
		-e HTTPS_KEY_FILE=/certs/tls.key \
		-e HTTPS_CERT_FILE=/certs/tls.crt \
		$(IMAGE)

.PHONY: certs
certs:
	openssl req -x509 -sha256 -nodes -days 365 -newkey rsa:2048 -keyout certs/tls.key -out certs/tls.crt -subj "/CN=localhost"

all: lint test build run

clean:
	-rm -rf $(REPORTS)
