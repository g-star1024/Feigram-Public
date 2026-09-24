require("telegram");
const { ConnectionTCPAbridged } = require("telegram/network/connection/TCPAbridged");

// Use Telegram's low-overhead abridged MTProto transport on TCP/443. Both the
// full and obfuscated variants are terminated by some SOCKS/transparent proxy
// paths immediately after their transport handshake.
class ConnectionTCPAbridged443 extends ConnectionTCPAbridged {
  constructor(options) {
    super({ ...options, port: 443 });
  }
}

module.exports = { ConnectionTCPAbridged443 };
