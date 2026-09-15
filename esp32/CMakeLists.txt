# navfeeder-esp — the navlistener ESP32 edge feeder (top-level ESP-IDF project).
# Native ESP-IDF v5.5.4 (no Arduino-as-component). Build with build-navfeeder-esp.sh.
cmake_minimum_required(VERSION 3.16)

if(DEFINED IDF_TARGET AND NOT IDF_TARGET STREQUAL "esp32s3")
    message(FATAL_ERROR "ESP32-C6 support is retired. Use the ESP32-S3 observer with 16 MiB flash and 8 MiB PSRAM.")
endif()

if(NOT DEFINED SDKCONFIG_DEFAULTS)
    if(DEFINED ENV{SDKCONFIG_DEFAULTS} AND NOT "$ENV{SDKCONFIG_DEFAULTS}" STREQUAL "")
        set(SDKCONFIG_DEFAULTS "$ENV{SDKCONFIG_DEFAULTS}")
    else()
        set(SDKCONFIG_DEFAULTS "sdkconfig.defaults;sdkconfig.defaults.s3")
    endif()
endif()

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
# The release adapter builds isolated clean commits. ESP-IDF may regenerate its
# target-specific lock; that derived file must not change the release identity.
if(DEFINED NVF_RELEASE_REVISION)
    string(LENGTH "${NVF_RELEASE_REVISION}" release_revision_length)
    execute_process(COMMAND git rev-parse HEAD WORKING_DIRECTORY "${CMAKE_CURRENT_SOURCE_DIR}"
        OUTPUT_VARIABLE actual_revision OUTPUT_STRIP_TRAILING_WHITESPACE COMMAND_ERROR_IS_FATAL ANY)
    if(NOT release_revision_length EQUAL 40 OR NOT NVF_RELEASE_REVISION MATCHES "^[0-9a-f]+$" OR NOT actual_revision STREQUAL NVF_RELEASE_REVISION)
        message(FATAL_ERROR "Release revision must equal this isolated checkout's exact commit")
    endif()
    execute_process(COMMAND git diff --quiet HEAD -- . ":(exclude)esp32/dependencies.lock"
        WORKING_DIRECTORY "${CMAKE_CURRENT_SOURCE_DIR}/.." COMMAND_ERROR_IS_FATAL ANY)
    string(SUBSTRING "${NVF_RELEASE_REVISION}" 0 7 NVF_REVISION)
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
if(NOT IDF_TARGET STREQUAL "esp32s3")
    message(FATAL_ERROR "Supported hardware is the ESP32-S3 observer with 16 MiB flash and 8 MiB PSRAM. Use build-navfeeder-esp.sh with a fresh build directory; ESP32-C6 support is retired.")
endif()
if(NOT CONFIG_NVF_BOARD_GNSS_COLOR_NEO OR NOT CONFIG_ESPTOOLPY_FLASHSIZE_16MB OR
   NOT CONFIG_SPIRAM OR NOT CONFIG_SPIRAM_MODE_OCT OR
   NOT CONFIG_PARTITION_TABLE_CUSTOM_FILENAME STREQUAL "partitions-s3.csv" OR
   NOT CONFIG_PARTITION_TABLE_OFFSET EQUAL 65536)
    message(FATAL_ERROR "The supported S3 observer requires its 16 MiB flash / octal PSRAM board profile and partition layout 3. Existing sdkconfig files retain old values; use a fresh build directory with build-navfeeder-esp.sh.")
endif()
