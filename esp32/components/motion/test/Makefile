# Host tests for the motion component: make -C esp32/components/motion/test
CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/icm45686_test
	./build/icm45686_test
build/icm45686_test: icm45686_test.c ../src/icm45686.c ../include/icm45686.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include icm45686_test.c ../src/icm45686.c -o $@
clean:
	rm -rf build
