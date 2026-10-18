CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/network_test
	./build/network_test ../../../../testdata/network_health_v1.hex
build/network_test: network_test.c ../src/network_health.c ../include/network_health.h ../../../../common/network_health.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include network_test.c ../src/network_health.c -o $@
clean:
	rm -f build/network_test
