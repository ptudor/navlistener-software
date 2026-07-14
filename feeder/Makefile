# navfeeder — the navlistener edge feeder (C).
#
# A near-verbatim port of radiolistener/feeder (the resilient spool/ack/replay core is
# reused; the source parser and on-wire record are GNSS-specific). Host build for dev/test:
# on macOS the user runs MacPorts, so OpenSSL + libzstd are under /opt/local; on Linux the
# system libraries are used. Override SSL_PREFIX as needed.
#
# The real fleet target is a tiny binary on the OpenWrt/SBC observers — the `openwrt`
# cross-build sketch below, wired up when the u-blox fleet (the .61 F9P, the F9T, and the
# rest of the observers) is converted from dial-mode ingest to authenticated push (P7).

BINARY    := navfeeder
CC        ?= cc
CFLAGS    ?= -O2 -Wall -Wextra -std=c11
# 32-bit glibc hosts (e.g. armhf Debian) default to a 32-bit off_t, so a spool file
# ≥ 2 GiB hits EFBIG on append and EOVERFLOW on fopen. Force 64-bit file offsets everywhere
# (harmless on musl/64-bit, which already use a 64-bit off_t).
override CFLAGS += -D_FILE_OFFSET_BITS=64
LDLIBS    := -lssl -lcrypto -lzstd -lpthread

UNAME := $(shell uname -s)
ifeq ($(UNAME),Darwin)
  SSL_PREFIX ?= /opt/local
  override CFLAGS += -I$(SSL_PREFIX)/include
  LDFLAGS += -L$(SSL_PREFIX)/lib
endif

.PHONY: all clean openwrt

all: $(BINARY)

$(BINARY): navfeeder.c
	$(CC) $(CFLAGS) $(LDFLAGS) -o $@ navfeeder.c $(LDLIBS)

clean:
	rm -f $(BINARY)

# OpenWrt cross-build. Dynamically links the libs OpenWrt already ships (OpenSSL 3 +
# libzstd); mips32 also needs libatomic for libcrypto's 64-bit atomics. Point at an OpenWrt
# buildroot's staging_dir and name the target + gcc version (from the box's
# /etc/openwrt_release DISTRIB_ARCH and the toolchain-* dir):
#   make openwrt STAGING_DIR=/path/openwrt/staging_dir TARGET=mipsel_24kc_musl GCC_VER=15.2.0
# ARCH drops the _musl suffix (mipsel_24kc_musl -> mipsel_24kc); GCCPFX is the gcc prefix
# (mipsel-openwrt-linux). Keep these comments off the assignment lines: a trailing inline
# comment in a := assignment captures the spaces before # into the value.
TARGET   ?= mipsel_24kc_musl
GCC_VER  ?= 15.2.0
ARCH     := $(TARGET:_musl=)
GCCPFX   := $(firstword $(subst _, ,$(TARGET)))-openwrt-linux
TC       := $(STAGING_DIR)/toolchain-$(ARCH)_gcc-$(GCC_VER)_musl
SYS      := $(STAGING_DIR)/target-$(TARGET)
openwrt:
	@test -n "$(STAGING_DIR)" || { echo "set STAGING_DIR=<openwrt>/staging_dir"; exit 2; }
	STAGING_DIR=$(STAGING_DIR) $(TC)/bin/$(GCCPFX)-gcc \
		-Os -Wall -std=c11 -D_FILE_OFFSET_BITS=64 --sysroot=$(SYS) \
		-I$(SYS)/usr/include -L$(SYS)/usr/lib -L$(TC)/lib \
		-Wl,-rpath-link,$(SYS)/usr/lib -Wl,-rpath-link,$(TC)/lib \
		-o $(BINARY) navfeeder.c -lssl -lcrypto -lzstd -lpthread -latomic
