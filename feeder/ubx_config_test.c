/* Exercise the real reader and setup state machine without receiver hardware. */
#define main navfeeder_main
#include "navfeeder.c"
#undef main
#include <assert.h>

static void no_output(int fd) {
	struct pollfd p = { .fd = fd, .events = POLLIN };
	assert(poll(&p, 1, 0) == 0);
}

static void check_packet(int fd, int port) {
	unsigned char packet[32];
	assert(recv(fd, packet, sizeof packet, MSG_WAITALL) == sizeof packet);
	assert(!memcmp(packet, "\xb5\x62\x06\x8a\x18\x00\x00\x01\x00\x00", 10));
	/* Assert the entire key set: adding a persistent layer, baud or NMEA key
	 * must fail this test, even when the rest of the receiver setup still works. */
	assert(rd_le32(packet + 10) == (port == 1 ? 0x10740001u :
		port == 2 ? 0x10760001u : 0x10780001u));
	assert(rd_le32(packet + 15) == 0x20910231u + (unsigned)port);
	assert(rd_le32(packet + 20) == 0x20910015u + (unsigned)port);
	assert(rd_le32(packet + 25) == 0x20910359u + (unsigned)port);
	assert(packet[14] == 1 && packet[19] == 1 && packet[24] == 1 && packet[29] == 1);
	unsigned sum = 0, weighted = 0;
	for (unsigned i = 2; i < 30; i++) { sum += packet[i]; weighted += (30-i)*packet[i]; }
	assert(packet[30] == (sum & 255) && packet[31] == (weighted & 255));
}

static void test_setup_and_recovery(void) {
	int fd[2]; assert(socketpair(AF_UNIX, SOCK_STREAM, 0, fd) == 0);
	struct ubx_config c = {0};
	assert(ubx_config_tick(&c, fd[0], 100) == 0);
	no_output(fd[1]); /* passive sources never receive configuration */
	for (int port = 1; port <= 3; port++) {
		c = (struct ubx_config){ .port = port };
		int flags = fcntl(fd[0], F_GETFL);
		assert(ubx_config_tick(&c, fd[0], 100) == 0);
		assert((fcntl(fd[0], F_GETFL) & O_NONBLOCK) == (flags & O_NONBLOCK));
		check_packet(fd[1], port);
		assert(c.awaiting_ack);
		const unsigned char unrelated[] = {0x06, 0x01}, ack[] = {0x06, 0x8a};
		ubx_config_ack(&c, 5, 1, unrelated, 2);
		ubx_config_ack(&c, 5, 1, ack, 1);
		assert(c.awaiting_ack);
		ubx_config_ack(&c, 5, 1, ack, 2);
		assert(!c.awaiting_ack);
		/* Healthy output suppresses retries; ACK alone does not. */
		ubx_config_observed(&c, 125);
		assert(ubx_config_tick(&c, fd[0], 130) == 0);
		no_output(fd[1]);
		assert(ubx_config_tick(&c, fd[0], 155) == 0);
		check_packet(fd[1], port);
		/* A NAK leaves input capture and a bounded retry available. */
		ubx_config_ack(&c, 5, 0, ack, 2);
		assert(!c.awaiting_ack);
		assert(ubx_config_tick(&c, fd[0], 184) == 0);
		no_output(fd[1]);
		assert(ubx_config_tick(&c, fd[0], 185) == 0);
		check_packet(fd[1], port);
		assert(ubx_config_tick(&c, fd[0], 188) == 0);
		assert(!c.awaiting_ack);
		no_output(fd[1]);
	}
	/* A powered-down receiver behind a live UART bridge resumes NMEA only.
	 * Force the recovery deadline due and drive the real buffered sync scan. */
	struct rdbuf rb = { .fd = fd[0], .config = { .port = 1, .next_attempt = 1 } };
	const unsigned char nmea[] = "$GNTXT,01,01,02,boot*00\r\n\xb5\x62";
	assert(write(fd[1], nmea, sizeof nmea - 1) == sizeof nmea - 1);
	assert(sync_ubx(&rb) == 0);
	check_packet(fd[1], 1);
	close(fd[0]); close(fd[1]);
}

static void *receiver(void *arg) {
	int fd = *(int *)arg;
	check_packet(fd, 1);
	/* Interleave ACK and NAV-SAT with NMEA. A setup response must not consume
	 * or flush the navigation message needed by the ordinary spool path. */
	const unsigned char prefix[] = "$GNTXT,01,01,02,test*00\r\n\xb5\x62\x05\x01\x02\x00\x06\x8a\x98\xc1";
	assert(write(fd, prefix, sizeof prefix - 1) == sizeof prefix - 1);
	unsigned char sat[28] = {0xb5,0x62,0x01,0x35,20,0, 0,0,0,0,1,1,0,0,
		0,7,42,30,0,0,0,0,8,0,0,0,0,0};
	for (unsigned i = 2; i < 26; i++) { sat[26] += sat[i]; sat[27] += sat[26]; }
	assert(write(fd, sat, sizeof sat) == sizeof sat);
	assert(shutdown(fd, SHUT_WR) == 0);
	return NULL;
}

static void test_reader_delivery(void) {
	int fd[2]; assert(socketpair(AF_UNIX, SOCK_STREAM, 0, fd) == 0);
	assert(spool_init(&g_spool, 8, NULL, 0) == 0);
	pthread_t peer; assert(pthread_create(&peer, NULL, receiver, &fd[1]) == 0);
	assert(run_ubx(fd[0], 1));
	assert(pthread_join(peer, NULL) == 0);
	assert(g_spool.count == 1);
	struct frame *f = &g_spool.ring[g_spool.head];
	assert(f->data[12] == F_T_RECEPTION);
	assert(f->len == RECORD_HDR + 8);
	assert(f->data[RECORD_HDR + 4] == 7); /* SV7 survived setup/ACK */
	free(f->data);
	free(g_spool.ring);
	pthread_mutex_destroy(&g_spool.mu);
	close(fd[0]); close(fd[1]);
}

static void test_write_failure(void) {
	int fd[2]; assert(socketpair(AF_UNIX, SOCK_STREAM, 0, fd) == 0);
	int flags = fcntl(fd[0], F_GETFL);
	assert(fcntl(fd[0], F_SETFL, flags | O_NONBLOCK) == 0);
	char fill[4096] = {0};
	while (write(fd[0], fill, sizeof fill) > 0) {}
	assert(errno == EAGAIN || errno == EWOULDBLOCK);
	assert(fcntl(fd[0], F_SETFL, flags) == 0);
	unsigned char packet[32]; assert(ubx_config_packet(packet, 1) == sizeof packet);
	time_t start = monotonic_s();
	assert(ubx_config_write(fd[0], packet, sizeof packet) == -1);
	assert(errno == ETIMEDOUT && monotonic_s() - start <= 3);
	assert((fcntl(fd[0], F_GETFL) & O_NONBLOCK) == (flags & O_NONBLOCK));
	close(fd[0]); close(fd[1]);
}

int main(void) {
	assert(ubx_config_port("uart1") == 1 && ubx_config_port("uart2") == 2);
	assert(ubx_config_port("usb") == 3 && ubx_config_port("auto") == 0);
	test_setup_and_recovery();
	test_reader_delivery();
	test_write_failure();
	puts("UBX RAM setup, NMEA-only recovery, interleaved delivery and write timeout: PASS");
	return 0;
}
