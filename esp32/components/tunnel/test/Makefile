CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/tunnel_test
	./build/tunnel_test
build/tunnel_test: tunnel_test.c ../src/tunnel.c ../include/tunnel.h ../../netcfg/src/netcfg_tunnel.c $(wildcard stubs/*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -Istubs -I../include -I../../netcfg/include tunnel_test.c ../../netcfg/src/netcfg_tunnel.c -o $@
clean:
	rm -rf build
