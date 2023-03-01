FROM golang:1.17-alpine3.16 AS builder

WORKDIR /app

COPY . .

RUN go mod download

RUN go mod verify

RUN GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o vault-init

FROM alpine:3.16

WORKDIR /app

RUN apk --no-cache add tini

ENV APP_ENV production

ENV UID=10001

RUN addgroup -S vault-init-service

RUN adduser -D \    
	--disabled-password \    
	--gecos "" \    
	--home "/nonexistent" \    
	--shell "/sbin/nologin" \    
	--no-create-home \    
	--uid "${UID}" \    
	vault-init-user \ 
	-G vault-init-service

USER vault-init-user

COPY --chown=vault-init-user:vault-init-service --from=builder /app/vault-init /app/vault-init

ENTRYPOINT [ "/sbin/tini", "--" ]

CMD ["/app/vault-init"]
