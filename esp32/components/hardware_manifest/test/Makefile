CC ?= cc
CFLAGS ?= -std=c11 -Wall -Wextra -Werror -pedantic

TEST := hardware_manifest_policy_test
SRC := test_policy.c ../src/hardware_manifest_policy.c
INC := -I../include

.PHONY: all clean
all: $(TEST) build/checks_test build/identity_test build/manifest_board_test
	./$(TEST)
	./build/checks_test
	./build/identity_test
	./build/manifest_board_test

# The discovery library's header, compiled against stubs of the two ESP-IDF headers it includes.
DISCOVERY := ../../../managed_components/esp_hardware_discovery/include
build/manifest_board_test: manifest_board_test.c ../src/manifest_board.c ../include/manifest_board.h $(wildcard stubs/*.h stubs/driver/*.h)
	@mkdir -p build
	$(CC) $(CFLAGS) -Istubs -I$(DISCOVERY) $(INC) manifest_board_test.c ../src/manifest_board.c -o $@

build/checks_test: checks_test.c ../src/hardware_checks.c ../include/hardware_checks.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INC) checks_test.c ../src/hardware_checks.c -o $@

$(TEST): $(SRC)
	$(CC) $(CFLAGS) $(INC) $(SRC) -o $@

clean:
	rm -f $(TEST)
	rm -rf build

build/identity_test: identity_test.c ../../../../common/board_uid.h ../../../../common/board_identity.h \
		../src/hardware_manifest_policy.c ../include/hardware_manifest_policy.h
	@mkdir -p build
	$(CC) $(CFLAGS) $(INC) identity_test.c ../src/hardware_manifest_policy.c -o $@
