CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/ring_test
	./build/ring_test
build/ring_test: ring_test.c ../src/spool_ring.c ../include/spool_ring.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include ring_test.c ../src/spool_ring.c -o $@
clean:
	rm -rf build
