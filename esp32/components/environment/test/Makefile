CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
SOURCES := env_test.c ../src/env_sensors.c ../vendor/bmp3/bmp3.c
HEADERS := ../include/env_sensors.h ../vendor/bmp3/bmp3.h ../vendor/bmp3/bmp3_defs.h
.PHONY: all clean
all: build/env_test build/heater_test
	./build/env_test
	./build/heater_test
build/env_test: $(SOURCES) $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../vendor/bmp3 $(SOURCES) -lm -o $@
build/heater_test: heater_test.c ../src/env_heater.c ../include/env_heater.h $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../vendor/bmp3 heater_test.c ../src/env_heater.c -lm -o $@
clean:
	rm -rf build
