CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
SOURCES := rtc_test.c ../src/rtc_policy.c ../src/rtc_io.c ../../ubx/src/gnss_status.c
HEADERS := ../include/rtc_policy.h ../include/rtc_io.h ../../ubx/include/gnss_status.h
.PHONY: all clean
all: build/rtc_test build/max31328_test
	./build/rtc_test
	./build/max31328_test
build/rtc_test: $(SOURCES) $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../ubx/include $(SOURCES) -lm -o $@
build/max31328_test: max31328_test.c ../src/rtc_max31328.c ../src/rtc_policy.c ../../ubx/src/gnss_status.c ../include/rtc_max31328.h $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../ubx/include max31328_test.c ../src/rtc_max31328.c ../src/rtc_policy.c ../../ubx/src/gnss_status.c -lm -o $@
clean:
	rm -rf build
