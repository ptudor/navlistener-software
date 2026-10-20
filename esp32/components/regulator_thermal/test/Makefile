# Host tests for regulator_thermal: make -C esp32/components/regulator_thermal/test
CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
.PHONY: all clean
all: build/regulator_thermal_test
	./build/regulator_thermal_test
build/regulator_thermal_test: regulator_thermal_test.c ../src/regulator_thermal.c ../include/regulator_thermal.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include regulator_thermal_test.c ../src/regulator_thermal.c -o $@
clean:
	rm -rf build
