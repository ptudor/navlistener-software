# navfeeder-esp — the navlistener ESP32 edge feeder (top-level ESP-IDF project).
# Native ESP-IDF v5.4+ (no Arduino-as-component). Build with build-navfeeder-esp.sh,
# or: . ~/esp/esp-idf/export.sh && idf.py set-target esp32c6 && idf.py build
cmake_minimum_required(VERSION 3.16)

set(EXTRA_COMPONENT_DIRS "${CMAKE_CURRENT_SOURCE_DIR}/components")

# Human release/build numbers are advanced deliberately, not on each compile.
# Keep the exact revision in the 31-character app descriptor for journal/OTA.
file(STRINGS "${CMAKE_CURRENT_SOURCE_DIR}/../VERSION" NVF_RELEASE LIMIT_COUNT 1)
file(STRINGS "${CMAKE_CURRENT_SOURCE_DIR}/../BUILD_NUMBER" NVF_BUILD LIMIT_COUNT 1)
include($ENV{IDF_PATH}/tools/cmake/third_party/GetGitRevisionDescription.cmake)
# IDF's helper registers HEAD/ref dependencies, so committing also refreshes an
# existing build directory instead of retaining the prior configured revision.
git_describe(NVF_REVISION "${CMAKE_CURRENT_SOURCE_DIR}" --always --dirty --exclude=*)
if(NOT NVF_REVISION OR NVF_REVISION MATCHES "NOTFOUND")
    set(NVF_REVISION "unknown")
endif()
set(PROJECT_VER "${NVF_RELEASE}+${NVF_BUILD}.${NVF_REVISION}")
string(LENGTH "${PROJECT_VER}" NVF_VERSION_LENGTH)
if(NVF_VERSION_LENGTH GREATER 31)
    message(FATAL_ERROR "Release/build identity exceeds the 31-byte app version limit")
endif()
set_property(DIRECTORY APPEND PROPERTY CMAKE_CONFIGURE_DEPENDS
    "${CMAKE_CURRENT_SOURCE_DIR}/../VERSION" "${CMAKE_CURRENT_SOURCE_DIR}/../BUILD_NUMBER")

# Optional development override for the public discovery component. Normal
# clones resolve main/idf_component.yml from Git; local cross-repo work can use
# the uncommitted sibling checkout without baking a machine-specific path into
# this repository. The override directory's basename must be
# `esp_hardware_discovery`, matching
# the dependency key (a symlink is fine for an uncommitted sibling checkout).
#   ESP_HARDWARE_DISCOVERY_PATH=/tmp/esp_hardware_discovery idf.py build
if(DEFINED ENV{ESP_HARDWARE_DISCOVERY_PATH})
    list(APPEND EXTRA_COMPONENT_DIRS "$ENV{ESP_HARDWARE_DISCOVERY_PATH}")
endif()

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(navfeeder-esp)
