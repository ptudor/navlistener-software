CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/policy_test build/download_test
	./build/policy_test
	./build/download_test
	python3 ../../../tools/test_ota.py
build/policy_test: policy_test.c ../src/ota_policy.c ../include/ota_policy.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include policy_test.c ../src/ota_policy.c -o $@
build/download_test: download_test.c ../src/ota_download.c ../src/ota_policy.c ../../../../common/endpoint_fallback.h $(wildcard stubs/*.h stubs/*/*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -Istubs -I../include download_test.c ../src/ota_download.c ../src/ota_policy.c -o $@
clean:
	rm -rf build
