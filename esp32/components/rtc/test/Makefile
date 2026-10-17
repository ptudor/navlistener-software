CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
SOURCES := rtc_test.c ../src/rtc_policy.c ../src/rtc_io.c ../src/rtc_service.c ../src/rtc_max31328.c ../../ubx/src/gnss_status.c
HEADERS := ../include/rtc_policy.h ../include/rtc_io.h ../include/rtc_service.h ../include/observer_time_policy.h ../../ubx/include/gnss_status.h
.PHONY: all clean
all: build/rtc_test build/max31328_test build/time_policy_test
	./build/rtc_test
	./build/max31328_test
	./build/time_policy_test
build/rtc_test: $(SOURCES) $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../ubx/include $(SOURCES) -lm -o $@
build/time_policy_test: time_policy_test.c ../src/observer_time_policy.c ../include/observer_time_policy.h
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include time_policy_test.c ../src/observer_time_policy.c -o $@
build/max31328_test: max31328_test.c ../src/rtc_io.c ../src/rtc_service.c ../src/rtc_max31328.c ../src/rtc_policy.c ../../ubx/src/gnss_status.c ../include/rtc_max31328.h $(HEADERS)
	@mkdir -p build
	$(CC) $(CFLAGS) -I../include -I../../ubx/include max31328_test.c ../src/rtc_io.c ../src/rtc_service.c ../src/rtc_max31328.c ../src/rtc_policy.c ../../ubx/src/gnss_status.c -lm -o $@
clean:
	rm -rf build
