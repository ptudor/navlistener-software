CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
INCLUDES := -I../include -I../../ubx/include
SBF := ../src/sbf.c ../include/sbf.h
.PHONY: all clean
all: build/sbf_test build/sbf_status_test build/sbf_control_test
	./build/sbf_test
	./build/sbf_status_test
	./build/sbf_control_test
build/sbf_test: sbf_test.c sbf_build.h $(SBF)
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) sbf_test.c ../src/sbf.c -lm -o $@
build/sbf_status_test: sbf_status_test.c sbf_build.h $(SBF) ../src/sbf_status.c ../include/sbf_status.h ../../ubx/src/gnss_status.c ../../rtc/src/rtc_policy.c
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) -I../../rtc/include sbf_status_test.c ../src/sbf.c ../src/sbf_status.c ../../ubx/src/gnss_status.c ../../rtc/src/rtc_policy.c -lm -o $@
build/sbf_control_test: sbf_control_test.c sbf_build.h $(SBF) ../src/sbf_control.c ../include/sbf_control.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) sbf_control_test.c ../src/sbf.c ../src/sbf_control.c -lm -o $@
clean:
	rm -rf build
