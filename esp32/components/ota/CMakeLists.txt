idf_component_register(
    SRCS "src/ota.c" "src/ota_download.c" "src/ota_policy.c" "src/ota_image.c"
         "src/update_json.c" "src/update_tuf.c" "src/update_state.c" "src/update_api.c" "src/update_runtime.c"
    INCLUDE_DIRS "include"
    REQUIRES esp_http_server
    PRIV_REQUIRES app_update esp_http_client mbedtls nvs_flash esp_wifi
                  esp_timer esp_netif lwip esp_app_format bootloader_support json spool journal
)
set(NVF_TUF_ROOT_FILE "" CACHE FILEPATH "External public TUF trust root")
set(root_source "${CMAKE_CURRENT_BINARY_DIR}/update_root.c")
if(NVF_TUF_ROOT_FILE)
    set(root_flags "")
    if(CONFIG_NVF_UPDATE_TEST_KEYS)
        list(APPEND root_flags --test-only)
    endif()
    execute_process(COMMAND "${PYTHON}" "${CMAKE_CURRENT_SOURCE_DIR}/../../tools/embed_update_root.py"
        "${NVF_TUF_ROOT_FILE}" "${root_source}" ${root_flags} COMMAND_ERROR_IS_FATAL ANY)
    set_property(DIRECTORY APPEND PROPERTY CMAKE_CONFIGURE_DEPENDS "${NVF_TUF_ROOT_FILE}")
else()
    file(WRITE "${root_source}" "#include <stddef.h>\nconst char nvf_update_root[] = \"\";\nconst size_t nvf_update_root_length = 0;\n")
endif()
target_sources(${COMPONENT_LIB} PRIVATE "${root_source}")
file(STRINGS "${CMAKE_CURRENT_SOURCE_DIR}/../../../BUILD_NUMBER" update_build LIMIT_COUNT 1)
target_compile_definitions(${COMPONENT_LIB} PRIVATE "NVF_BUILD_NUMBER=${update_build}ULL")
target_link_libraries(${COMPONENT_LIB} INTERFACE "-u nvf_ota_metadata")
