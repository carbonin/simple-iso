FROM registry.ci.openshift.org/openshift/release:golang-1.19 as builder

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS="" GO111MODULE=on go build -o /image-config main.go

FROM quay.io/centos/centos:stream8

ARG DATA_DIR=/data
RUN mkdir $DATA_DIR && chmod 775 $DATA_DIR
VOLUME $DATA_DIR
ENV DATA_DIR=$DATA_DIR

COPY --from=builder /image-config /image-config
CMD ["/image-config"]
