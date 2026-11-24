# The client in client/ and models/ is generated from api/swagger.json and committed, so users need no generator.
SWAGGER_IMAGE ?= quay.io/goswagger/swagger:v0.31.0
GO_IMAGE      ?= golang:1.25

.PHONY: generate check-generated test
generate:
	rm -rf client models
	docker run --rm --user $$(id -u):$$(id -g) -e HOME=/tmp -v "$(CURDIR)":/work -w /work $(SWAGGER_IMAGE) \
	  generate client -q -f api/swagger.json -A fleet-telemetry --client-package=client --model-package=models -t .

# Regenerates and fails if the committed code differs from what the spec gives.
check-generated: generate
	git diff --exit-code -- client models
	test -z "$$(git status --porcelain -- client models)"

test:
	go vet ./... && go test ./...
