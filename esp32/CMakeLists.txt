# navfeeder-esp — the navlistener ESP32 edge feeder (top-level ESP-IDF project).
# Native ESP-IDF v5.4+ (no Arduino-as-component). Build with build-navfeeder-esp.sh,
# or: . ~/esp/esp-idf/export.sh && idf.py set-target esp32c6 && idf.py build
cmake_minimum_required(VERSION 3.16)

set(EXTRA_COMPONENT_DIRS "${CMAKE_CURRENT_SOURCE_DIR}/components")

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
