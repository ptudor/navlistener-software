#include "network_monitor.h"
#include "network_health.h"
#include "sdkconfig.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "esp_timer.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "gnf1.h"
#include "spool.h"
#include <string.h>
#include <sys/socket.h>
#include <netinet/in.h>
#if CONFIG_NVF_ETHERNET_W5500
#include "ethernet.h"
#endif
static uint64_t (*clock_ns)(void);
static uint8_t interface_kind(esp_netif_t *netif)
{
    if(!netif)return NH_UNKNOWN;
    if(netif==esp_netif_get_handle_from_ifkey("WIFI_STA_DEF"))return NH_WIFI;
    if(netif==esp_netif_get_handle_from_ifkey("ETH_DEF"))return NH_ETHERNET;
    return NH_OTHER;
}
static bool ip_info(esp_netif_t *netif,uint8_t out[12])
{
    esp_netif_ip_info_t ip={0};
    if(!netif || !esp_netif_is_netif_up(netif) || esp_netif_get_ip_info(netif,&ip)!=ESP_OK || !ip.ip.addr)return false;
    memcpy(out,&ip.ip.addr,4);memcpy(out+4,&ip.netmask.addr,4);memcpy(out+8,&ip.gw.addr,4);return true;
}
uint8_t network_monitor_socket(int fd,uint8_t peer[4])
{
    struct sockaddr_storage addr={0};socklen_t size=sizeof addr;
    if(getpeername(fd,(struct sockaddr *)&addr,&size)==0 && addr.ss_family==AF_INET)
        memcpy(peer,&((struct sockaddr_in *)&addr)->sin_addr,4);
    size=sizeof addr;
    if(getsockname(fd,(struct sockaddr *)&addr,&size)!=0 || addr.ss_family!=AF_INET)return NH_UNKNOWN;
    uint8_t local[12];
    if(ip_info(esp_netif_get_handle_from_ifkey("WIFI_STA_DEF"),local) && !memcmp(local,&((struct sockaddr_in *)&addr)->sin_addr,4))return NH_WIFI;
    if(ip_info(esp_netif_get_handle_from_ifkey("ETH_DEF"),local) && !memcmp(local,&((struct sockaddr_in *)&addr)->sin_addr,4))return NH_ETHERNET;
    return NH_OTHER;
}
static void monitor(void *arg)
{
    (void)arg;
    // One low-priority writer owns these buffers. No per-sample allocation.
    static nh_report_t previous,sample,report;
    static uint8_t body[27+NH_WIRE_SIZE],record[GNF1_RECORD_HDR+27+NH_WIRE_SIZE];
    nh_policy_t policy={0};uint64_t next_sample=0;
    for(;;){
        uint64_t now=esp_timer_get_time()/1000;
        size_t queued;uint64_t dropped;spool_stats(NULL,&dropped,&queued);
        network_health_spool((uint32_t)queued,dropped);
        if(now>=next_sample){
            network_health_read(&previous);memset(&sample,0,sizeof sample);
            esp_netif_t *def=esp_netif_get_default_netif();sample.default_interface=interface_kind(def);
            if(previous.flags&NH_WIFI_UP){
                wifi_ap_record_t ap;
                if(esp_wifi_sta_get_ap_info(&ap)==ESP_OK){sample.valid|=NH_RSSI_VALID;sample.rssi=ap.rssi;
                    sample.channel=ap.primary;sample.secondary_channel=ap.second;memcpy(sample.bssid,ap.bssid,6);
                }else sample.sample_failures++;
            }
            if(previous.flags&NH_WIFI_IP){if(ip_info(esp_netif_get_handle_from_ifkey("WIFI_STA_DEF"),sample.wifi_ipv4))sample.valid|=NH_WIFI_IP_VALID;else sample.sample_failures++;}
            if(previous.flags&NH_ETH_IP){if(ip_info(esp_netif_get_handle_from_ifkey("ETH_DEF"),sample.eth_ipv4))sample.valid|=NH_ETH_IP_VALID;else sample.sample_failures++;}
            for(unsigned i=0;i<2;i++){
                esp_netif_dns_info_t dns={0};
                if(def && esp_netif_get_dns_info(def,(esp_netif_dns_type_t)i,&dns)==ESP_OK && dns.ip.type==ESP_IPADDR_TYPE_V4){
                    memcpy(sample.dns_ipv4+4*i,&dns.ip.u_addr.ip4.addr,4);sample.valid|=NH_DNS_VALID;
                }
            }
#if CONFIG_NVF_ETHERNET_W5500
            if((previous.flags&NH_ETH_UP) && !ethernet_diagnostics(&sample.speed_mbps,&sample.duplex,&sample.autoneg))sample.sample_failures++;
#endif
            network_health_sample(&sample,previous.event_total,(uint64_t)(esp_timer_get_time()/1000));
            next_sample=now+10000;
        }
        network_health_read(&report);now=esp_timer_get_time()/1000;
        if(network_health_due(&policy,&report,now)){
            memset(body,0,27);body[0]=1;body[1]=policy.sent ? (report.event_total!=policy.events?2:4) : 1;
            gnf1_be64(body+2,now);body[24]=NH_TAG;gnf1_be16(body+25,NH_WIRE_SIZE);
            nh_encode(body+27,NH_WIRE_SIZE,&report);
            size_t n=gnf1_encode_telem(record,clock_ns?clock_ns():0,GNF1_T_OBSERVER,body,sizeof body);
            if(n && spool_try_append(record,(uint32_t)n))network_health_sent(&policy,&report,now);
        }
        vTaskDelay(pdMS_TO_TICKS(2000));
    }
}
void network_monitor_start(uint64_t (*utc_ns)(void))
{
    clock_ns=utc_ns;
    if(xTaskCreate(monitor,"net-health",4096,NULL,2,NULL)!=pdPASS)
        ESP_LOGW("network","diagnostics task unavailable");
}
