# Host tests for sensor_settings: make -C esp32/components/sensor_settings/test
CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/sensor_settings_test
	./build/sensor_settings_test
build/sensor_settings_test: sensor_settings_test.c ../src/sensor_settings.c ../include/sensor_settings.h $(wildcard mocks/*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -Imocks -I../include sensor_settings_test.c ../src/sensor_settings.c -o $@
clean:
	rm -rf build
