CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
SOURCES := env_test.c ../src/env_sensors.c ../vendor/bmp3/bmp3.c
HEADERS := ../include/env_sensors.h ../vendor/bmp3/bmp3.h ../vendor/bmp3/bmp3_defs.h
.PHONY: all clean
all: build/env_test build/heater_test build/ms5607_test build/max31856_test build/mmc34160_test
	./build/env_test
	./build/heater_test
	./build/ms5607_test
	./build/max31856_test
	./build/mmc34160_test
build/env_test: $(SOURCES) $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../vendor/bmp3 $(SOURCES) -lm -o $@
build/heater_test: heater_test.c ../src/env_heater.c ../include/env_heater.h $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../vendor/bmp3 heater_test.c ../src/env_heater.c -lm -o $@
build/ms5607_test: ms5607_test.c ../src/ms5607.c ../include/ms5607.h $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../vendor/bmp3 ms5607_test.c ../src/ms5607.c -o $@
build/max31856_test: max31856_test.c ../src/max31856.c ../include/max31856.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include max31856_test.c ../src/max31856.c -o $@
build/mmc34160_test: mmc34160_test.c ../src/mmc34160.c ../include/mmc34160.h $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../vendor/bmp3 mmc34160_test.c ../src/mmc34160.c -o $@
clean:
	rm -rf build
