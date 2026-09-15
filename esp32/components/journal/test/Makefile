CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/journal_test
	./build/journal_test
build/journal_test: journal_test.c ../src/journal_store.c ../include/journal_store.h ../include/journal_policy.h mocks/nvs.h
	@mkdir -p build
	$(CC) $(CFLAGS) -Imocks -I../../netcfg/test/mocks -I../include journal_test.c ../src/journal_store.c -o $@
clean:
	rm -rf build
