BUILD_PATH ?= ./build
#GOPATH ?= ${GOPATH}
IMAGE ?= influxdb
GOARCH ?= amd64
GOOS ?= linux
BRANCH ?= 1.8.10
COMMIT ?= $(shell git rev-parse HEAD)
VERSION ?= 1.8.10+bk
LDFLAGS = "-X main.version=$(VERSION) -X main.branch=$(BRANCH) -X main.commit=$(COMMIT)"


# build
.PHONY: build
build: clean tidy
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags $(LDFLAGS) -o $(BUILD_PATH)/ ./cmd/...

.PHONY: debug
debug: clean tidy
	go build -ldflags $(LDFLAGS) -o $(BUILD_PATH)/ ./cmd/...
	./bin/influxd -config ./influxdb.conf

.PHONY: tidy
tidy:
	@go mod tidy

.PHONY: docker
docker: build
	docker build -t $(IMAGE):$(VERSION) -f ./DockerfileRaw .

.PHONY: push
push: docker
	# influxdb:1.8.6-alpine
	docker push $(IMAGE):$(VERSION)

.PHONY: clean
clean:
ifneq ($(wildcard $(BUILD_PATH)), )
	@rm -rf $(BUILD_PATH)
endif

.PHONY: rpm
rpm:
	python build.py --outdir $(BUILD_PATH) --arch $(GOARCH) --platform $(GOOS) --branch $(BRANCH) --commit $(COMMIT) --version $(VERSION) --release --package

AB_H ?= influxdb.bktencent.com
AB_N ?= 5
AB_C ?= 1
AB_T ?= raw
AB_D ?= system
AB_M ?= cpu_detail
AB_F ?= usage
AB_E ?= 1d
AB_Accept ?= application/x-protobuf
AB_Accept_Encoding ?= snappy
AB_WHERE = time%3Enow()-$(AB_E)

ifeq ($(AB_T), raw)
    AB_URL = http://$(AB_H)/api/v1/raw/read?db=$(AB_D)&measurement=$(AB_M)&field=$(AB_F)&where=$(AB_WHERE)
else
    AB_URL = http://$(AB_H)/query?db=$(AB_D)&q=select%20$(AB_F),*::VERSION%20from%20$(AB_M)%20where%20$(AB_WHERE)
endif

.PHONY: ab
ab:
	ab -n $(AB_N) -c $(AB_C) -H "Accept: $(AB_Accept)" -H "Accept-Encoding: $(AB_Accept_Encoding)" "$(AB_URL)";