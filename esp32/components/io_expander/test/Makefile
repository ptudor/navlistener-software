CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/io_expander_test
	./build/io_expander_test
build/io_expander_test: io_expander_test.c ../src/io_expander.c ../include/io_expander.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include io_expander_test.c ../src/io_expander.c -o $@
clean:
	rm -rf build
