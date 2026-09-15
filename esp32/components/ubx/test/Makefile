CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/probe_test build/status_test
	./build/probe_test
	./build/status_test
build/status_test: status_test.c ../src/gnss_status.c ../include/gnss_status.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include status_test.c ../src/gnss_status.c -lm -o $@
build/probe_test: probe_test.c ../src/ubx.c ../src/ubx_probe.c ../include/ubx_probe.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../gnf1/include probe_test.c ../src/ubx.c ../src/ubx_probe.c ../../gnf1/src/gnf1.c -o $@
clean:
	rm -rf build
