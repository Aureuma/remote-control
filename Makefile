.PHONY: build check test test-browser

build:
	./scripts/build-guard.sh cargo build --workspace --jobs 2

check:
	./scripts/build-guard.sh cargo check --workspace --all-targets --all-features --jobs 2

test:
	./scripts/build-guard.sh cargo test --workspace --all-features --jobs 2

test-browser:
	./scripts/build-guard.sh corepack pnpm test:browser
