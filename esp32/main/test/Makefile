CC ?= cc
CFLAGS ?= -O1 -g -Wall -Wextra -Werror -std=c11
INCLUDES := -I.. -Istubs -I../../components/journal/test/mocks -I../../components/netcfg/test/mocks -I../../components/ubx/include -I../../components/gnf1/include -I../../components/observer_report/include -I../../components/journal/include -I../../components/spool/include -I../../components/pusher/include
# Stack budget guard. reception_poll runs on the board task every pass; its wire scratch
# and decoded candidates are static, so the frame holds only the decoders' temporaries
# and the samples (3.4 KB with host clang, 7.8 KB before the scratch moved). Host figures
# are not xtensa figures, but a change that puts that scratch back on the stack fails here.
STACK_LIMIT_reception_poll := 4096
.PHONY: all clean
all: build/reception_runtime_test build/power_history_test build/reception_runtime.su
	./build/reception_runtime_test
	./build/power_history_test
	@awk -F'\t' -v limit=$(STACK_LIMIT_reception_poll) '$$1 ~ /:reception_poll$$/ { found = 1; \
		if ($$2 + 0 > limit) { print "reception_poll frame " $$2 " bytes exceeds the " limit "-byte limit"; exit 1 } \
		print "reception_poll frame " $$2 " bytes (limit " limit ")" } \
		END { if (!found) { print "no reception_poll entry in build/reception_runtime.su"; exit 1 } }' build/reception_runtime.su
build/reception_runtime_test: reception_runtime_test.c ../reception_runtime.c ../reception_runtime.h ../power_history.c ../power_history.h ../../../common/reception.h ../../../common/reception_power.h ../../components/ubx/src/gnss_status.c
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) $< ../power_history.c ../../components/ubx/src/gnss_status.c ../../components/gnf1/src/gnf1.c -lm -o $@
build/power_history_test: power_history_test.c ../power_history.c ../power_history.h ../../../common/reception_power.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) $< ../power_history.c -lm -o $@
# Both gcc and clang write build/reception_runtime.su beside the object.
build/reception_runtime.su: ../reception_runtime.c ../reception_runtime.h ../../../common/reception.h ../../../common/reception_power.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INCLUDES) -fstack-usage -c ../reception_runtime.c -o build/reception_runtime.o
clean:
	rm -rf build
