# Host tests for receiver_settings: make -C esp32/components/receiver_settings/test
CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/receiver_settings_test
	./build/receiver_settings_test
build/receiver_settings_test: receiver_settings_test.c ../src/receiver_settings.c ../include/receiver_settings.h $(wildcard mocks/*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -Imocks -I../include receiver_settings_test.c ../src/receiver_settings.c -o $@
clean:
	rm -rf build
