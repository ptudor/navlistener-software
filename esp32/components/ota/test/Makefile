CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean tuf-client
all: build/policy_test build/download_test build/update_state_test
	./build/policy_test
	./build/download_test
	./build/update_state_test
	python3 ../../../tools/test_ota.py
build/policy_test: policy_test.c ../src/ota_policy.c ../include/ota_policy.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include policy_test.c ../src/ota_policy.c -o $@
build/download_test: download_test.c ../src/ota_download.c ../src/ota_policy.c ../../../../common/endpoint_fallback.h $(wildcard stubs/*.h stubs/*/*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -Istubs -I../include download_test.c ../src/ota_download.c ../src/ota_policy.c -o $@
tuf-client: build/update_tuf_test
build/update_state_test: update_state_test.c ../src/update_state.c ../src/update_api.c ../src/update_json.c ../src/ota_policy.c $(wildcard ../include/update_*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include update_state_test.c ../src/update_state.c ../src/update_api.c ../src/update_json.c ../src/ota_policy.c -o $@
build/update_tuf_test: update_tuf_test.c ../src/update_tuf.c ../src/update_json.c ../src/ota_policy.c $(wildcard ../include/update_*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) $$(pkg-config --cflags openssl) -I../include update_tuf_test.c ../src/update_tuf.c ../src/update_json.c ../src/ota_policy.c $$(pkg-config --libs openssl) -o $@
clean:
	rm -rf build
