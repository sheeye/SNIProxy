package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"regexp"

	"gopkg.in/yaml.v2"
)

var (
	version string // 编译时写入版本号

	ConfigFilePath string // 配置文件
	LogFilePath string // 日志文件
	EnableDebug bool   // 调试模式（详细日志）

	cfg configModel // 配置文件结构
	rulemap map[string]string //所有Rule的集合
	targetmap map[string]string //转发目标替换集合
)

// 配置文件结构
type configModel struct {
	ForwardRules  []Rule    `yaml:"rules,omitempty"`
	ListenAddrs   []string  `yaml:"listen_addrs,omitempty"`
	TargetMapping []Mapping `yaml:"target_mapping,omitempty"`
	EnableSocks   bool      `yaml:"enable_socks5,omitempty"`
	SocksAddr     string    `yaml:"socks_addr,omitempty"`
	AllowAllHosts bool      `yaml:"allow_all_hosts,omitempty"`
}

type Rule struct {
	Host   string `yaml:"host,omitempty"`
	Target string `yaml:"target,omitempty"`
}

type Mapping struct {
	Old string `yaml:"old,omitempty"`
	New string `yaml:"new,omitempty"`
}

func init() {
	var printVersion bool
	var help = `
SNIProxy ` + version + `
https://github.com/XIU2/SNIProxy

参数：
    -c config.yaml
        配置文件 (默认 config.yaml)
    -l sni.log
        日志文件 (默认 无)
    -d
        调试模式 (默认 关)
    -v
        程序版本
    -h
        帮助说明
`
	flag.StringVar(&ConfigFilePath, "c", "./config.yaml", "配置文件")
	flag.StringVar(&LogFilePath, "l", "", "日志文件")
	flag.BoolVar(&EnableDebug, "d", false, "调试模式")
	flag.BoolVar(&printVersion, "v", false, "程序版本")
	flag.Usage = func() { fmt.Print(help) }
	flag.Parse()
	if printVersion {
		fmt.Printf("XIU2/SNIProxy %s\n", version)
		os.Exit(0)
	}
}

func main() {
	data, err := os.ReadFile(ConfigFilePath) // 读取配置文件
	if err != nil {
		serviceLogger(fmt.Sprintf("配置文件读取失败: %v", err), 31, false)
		os.Exit(1)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		serviceLogger(fmt.Sprintf("配置文件解析失败: %v", err), 31, false)
		os.Exit(1)
	}
	if len(cfg.ForwardRules) <= 0 && !cfg.AllowAllHosts { // 如果 rules 为空且 allow_all_hosts 不等于 true
		serviceLogger("配置文件中 rules 不能为空（除非 allow_all_hosts 等于 true）!", 31, false)
		os.Exit(1)
	}
	serviceLogger(fmt.Sprintf("当前版本: %v", version), 32, false)
	serviceLogger(fmt.Sprintf("调试模式: %v", EnableDebug), 32, false)
	serviceLogger(fmt.Sprintf("前置代理: %v", cfg.EnableSocks), 32, false)
	serviceLogger(fmt.Sprintf("任意域名: %v", cfg.AllowAllHosts), 32, false)
	rulemap = make(map[string]string)
	for _, rule := range cfg.ForwardRules { // 输出规则中的所有域名
		rulemap[strings.ToLower(rule.Host)] = rule.Target
		if len(rule.Target) == 0 {
			serviceLogger(fmt.Sprintf("加载规则: %v", rule.Host), 32, false)
		} else {
			serviceLogger(fmt.Sprintf("加载规则: %v -> %v", rule.Host, rule.Target), 32, false)
		}
	}
	targetmap = make(map[string]string)
	for _, mapping := range cfg.TargetMapping { //输出目标映射
		targetmap[mapping.Old] = mapping.New
		serviceLogger(fmt.Sprintf("目标映射: %v -> %v", mapping.Old, mapping.New), 32, false)
	}

	startSniProxy() // 启动 SNI Proxy
}

// 启动 SNI Proxy
func startSniProxy() {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, ListenAddr := range cfg.ListenAddrs {
		listener, err := net.Listen("tcp", ListenAddr)
		if err != nil {
			serviceLogger(fmt.Sprintf("监听失败(" + ListenAddr + "): %v", err), 31, false)
			continue
		}
		serviceLogger(fmt.Sprintf("开始监听: %v", listener.Addr()), 0, false)
		go listen(listener)
	}
	
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	s := <-ch
	cancel()
	fmt.Printf("\n接收到信号 %s, 退出.\n", s)
}

func listen(listener net.Listener) {
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	for {
		connection, err := listener.Accept()
		if err != nil {
			serviceLogger(fmt.Sprintf("接受连接请求时出错: %v", err), 31, false)
			continue
		}
		raddr := connection.RemoteAddr().(*net.TCPAddr)
		serviceLogger("连接来自: "+raddr.String(), 32, false)
		go serve(connection, raddr.String(), port) // 有新连接进来，启动一个新线程处理
	}
}

// 处理新连接
func serve(c net.Conn, raddr string, port int) {
	defer c.Close()

	buf := make([]byte, 1024) // 分配缓冲区
	n, err := c.Read(buf)     // 读入新连接的内容
	if err != nil && fmt.Sprintf("%v", err) != "EOF" {
		serviceLogger(fmt.Sprintf("读取连接请求时出错: %v", err), 31, false)
		return
	}

	if n == 0 {
		serviceLogger("无数据传入, 忽略...", 31, false)
		return
	}

	target := "";
	ok := false;
	if len(rulemap) == 1 {
		target, ok = rulemap["*"]
		if ok && len(target) > 0 {
			if(!strings.Contains(target, ":")) {
				target = fmt.Sprintf("%s:%d", target, port)
			}
			serviceLogger(fmt.Sprintf("转发目标: %s", target), 32, false)
			forward(c, buf[:n], target, raddr)
			return
		}
	}
	
	RequestType := getRequestType(buf[:n])
	ServerName := ""
	if RequestType == "HTTP" {
		ServerName = getHTTPServerName(buf[:n])
	} else {
		ServerName = getSNIServerName(buf[:n]) // 获取 SNI 域名
	}

	if ServerName == "" {
		serviceLogger("未找到域名, 忽略...", 31, false)
		return
	}

	target = "";
	ok = false;
	host := strings.ToLower(ServerName)
	target, ok = rulemap[host]
	if !ok {
		hostarr := strings.Split(host, ".")
		for i := 1; i < len(hostarr); i++ {
			host = strings.Join(hostarr[i:], ".")
			target, ok = rulemap[host]
			if ok {
				break
			}
		}
	}
	if !ok {
		target, ok = rulemap["*"]
	}
	if !ok {
		if !cfg.AllowAllHosts {
			return
		}
		target = ""
	}
	if len(target) == 0 {
		target = fmt.Sprintf("%s:%d", ServerName, port)
	} else {
		if(!strings.Contains(target, ":")) {
			target = fmt.Sprintf("%s:%d", target, port)
		}
	}

	mapping := ""
	mapping, ok = targetmap[target]
	if ok {
		target = mapping
	}
	
	serviceLogger(fmt.Sprintf("转发目标: %s", target), 32, false)
	forward(c, buf[:n], target, raddr)
}

func getRequestType(buf []byte) string {
	n := len(buf)
	if n < 5 {
		return "HTTP"
	}
	if recordType(buf[0]) != recordTypeHandshake {
		return "HTTP"
	}
	if buf[5] != typeClientHello {
		return "HTTP"
	}
	return "HTTPS"
}

func getHTTPServerName(buf []byte) string {
	txt := string(buf)
	reg := regexp.MustCompile(`(?i)[\r\n]Host:\s*([A-Za-z0-9\-\.]+)[:\r\n]`)
	match := reg.FindStringSubmatch(txt)
	if match == nil {
		serviceLogger("未匹配到Host", 31, true)
		return ""
	}
	return match[1]
}

// 获取 SNI 域名
func getSNIServerName(buf []byte) string {
	n := len(buf)
	if n < 5 {
		serviceLogger("不是 TLS 握手消息", 31, true)
		return ""
	}

	// TLS 记录类型
	if recordType(buf[0]) != recordTypeHandshake {
		serviceLogger("不是 TLS", 31, true)
		return ""
	}

	// TLS 主要版本
	if buf[1] != 3 {
		serviceLogger("不支持 TLS 版本 < 3", 31, true)
		return ""
	}

	// payload 长度
	//l := int(buf[3])<<16 + int(buf[4])
	//log.Printf("length: %d, got: %d", l, n)

	// 握手消息类型
	if buf[5] != typeClientHello {
		serviceLogger("不是 Client Hello 消息", 31, true)
		return ""
	}

	// 以下开始解析 Client Hello 消息
	msg := &clientHelloMsg{}
	// Client Hello 不包含 TLS 标头, 5 字节
	ret := msg.unmarshal(buf[5:n])
	if !ret {
		serviceLogger("解析 Client Hello 消息失败", 31, true)
		return ""
	}
	return msg.serverName
}

// forward 函数接收一个 net.Conn 类型的连接对象 conn、一个 []byte 类型的数据 data、一个目标地址 dst、一个来源地址 raddr
// 该函数使用 GetDialer 函数创建一个与目标地址 dst 的后端连接 backend，将 data 写入 backend，然后使用 ioReflector 函数将 backend 和 conn 连接起来，以便将数据从一个连接转发到另一个连接
func forward(conn net.Conn, data []byte, dst string, raddr string) {
	backend, err := GetDialer(cfg.EnableSocks).Dial("tcp", dst)
	if err != nil {
		serviceLogger(fmt.Sprintf("无法连接到后端, %v", err), 31, false)
		return
	}

	defer backend.Close()

	if _, err = backend.Write(data); err != nil {
		serviceLogger(fmt.Sprintf("无法传输到后端, %v", err), 31, false)
		return
	}

	go ioReflector(conn, backend, true, raddr, dst)
	ioReflector(backend, conn, false, dst, raddr)
}

// ioReflector 函数接收一个 io.WriteCloser 类型的写入对象 dst、一个 io.Reader 类型的读取对象 src、一个 bool 类型的 isToClient，以及两个字符串类型的 dstaddr 和 srcaddr
// 该函数使用 io.Copy 函数将 src 中读取到的数据流复制到 dst 中，然后将转发的字节数写入日志
func ioReflector(dst io.WriteCloser, src io.Reader, isToClient bool, dstaddr string, srcaddr string) {
	// 将 IO 流反映到另一个
	written, _ := io.Copy(dst, src)
	serviceLogger(fmt.Sprintf("[%v] -> [%v] %d bytes", srcaddr, dstaddr, written), 33, true)
}

// 解析 Client Hello 消息
func (m *clientHelloMsg) unmarshal(data []byte) bool {
	if len(data) < 42 {
		return false
	}
	m.raw = data
	m.vers = uint16(data[4])<<8 | uint16(data[5])
	m.random = data[6:38]
	sessionIDLen := int(data[38])
	if sessionIDLen > 32 || len(data) < 39+sessionIDLen {
		return false
	}
	m.sessionID = data[39 : 39+sessionIDLen]
	data = data[39+sessionIDLen:]
	if len(data) < 2 {
		return false
	}
	// cipherSuiteLen 是密码套件编号的字节数。由于是 uint16，因此数字必须是偶数
	cipherSuiteLen := int(data[0])<<8 | int(data[1])
	if cipherSuiteLen%2 == 1 || len(data) < 2+cipherSuiteLen {
		return false
	}
	numCipherSuites := cipherSuiteLen / 2
	m.cipherSuites = make([]uint16, numCipherSuites)
	for i := 0; i < numCipherSuites; i++ {
		m.cipherSuites[i] = uint16(data[2+2*i])<<8 | uint16(data[3+2*i])
		if m.cipherSuites[i] == scsvRenegotiation {
			m.secureRenegotiationSupported = true
		}
	}
	data = data[2+cipherSuiteLen:]
	if len(data) < 1 {
		return false
	}
	compressionMethodsLen := int(data[0])
	if len(data) < 1+compressionMethodsLen {
		return false
	}
	m.compressionMethods = data[1 : 1+compressionMethodsLen]

	data = data[1+compressionMethodsLen:]

	m.nextProtoNeg = false
	m.serverName = ""
	m.ocspStapling = false
	m.ticketSupported = false
	m.sessionTicket = nil
	m.signatureAndHashes = nil
	m.alpnProtocols = nil
	m.scts = false

	if len(data) == 0 {
		// ClientHello 后面可选地跟着扩展数据
		return true
	}
	if len(data) < 2 {
		return false
	}

	extensionsLength := int(data[0])<<8 | int(data[1])
	data = data[2:]
	if extensionsLength != len(data) {
		return false
	}

	for len(data) != 0 {
		if len(data) < 4 {
			return false
		}
		extension := uint16(data[0])<<8 | uint16(data[1])
		length := int(data[2])<<8 | int(data[3])
		data = data[4:]
		if len(data) < length {
			return false
		}

		switch extension {
		case extensionServerName:
			d := data[:length]
			if len(d) < 2 {
				return false
			}
			namesLen := int(d[0])<<8 | int(d[1])
			d = d[2:]
			if len(d) != namesLen {
				return false
			}
			for len(d) > 0 {
				if len(d) < 3 {
					return false
				}
				nameType := d[0]
				nameLen := int(d[1])<<8 | int(d[2])
				d = d[3:]
				if len(d) < nameLen {
					return false
				}
				if nameType == 0 {
					m.serverName = string(d[:nameLen])
					// SNI 值末尾不能有点
					if strings.HasSuffix(m.serverName, ".") {
						return false
					}
					break
				}
				d = d[nameLen:]
			}
		case extensionNextProtoNeg:
			if length > 0 {
				return false
			}
			m.nextProtoNeg = true
		case extensionStatusRequest:
			m.ocspStapling = length > 0 && data[0] == statusTypeOCSP
		case extensionSupportedCurves:
			// http://tools.ietf.org/html/rfc4492#section-5.5.1
			if length < 2 {
				return false
			}
			l := int(data[0])<<8 | int(data[1])
			if l%2 == 1 || length != l+2 {
				return false
			}
			numCurves := l / 2
			m.supportedCurves = make([]CurveID, numCurves)
			d := data[2:]
			for i := 0; i < numCurves; i++ {
				m.supportedCurves[i] = CurveID(d[0])<<8 | CurveID(d[1])
				d = d[2:]
			}
		case extensionSupportedPoints:
			// http://tools.ietf.org/html/rfc4492#section-5.5.2
			if length < 1 {
				return false
			}
			l := int(data[0])
			if length != l+1 {
				return false
			}
			m.supportedPoints = make([]uint8, l)
			copy(m.supportedPoints, data[1:])
		case extensionSessionTicket:
			// http://tools.ietf.org/html/rfc5077#section-3.2
			m.ticketSupported = true
			m.sessionTicket = data[:length]
		case extensionSignatureAlgorithms:
			// https://tools.ietf.org/html/rfc5246#section-7.4.1.4.1
			if length < 2 || length&1 != 0 {
				return false
			}
			l := int(data[0])<<8 | int(data[1])
			if l != length-2 {
				return false
			}
			n := l / 2
			d := data[2:]
			m.signatureAndHashes = make([]signatureAndHash, n)
			for i := range m.signatureAndHashes {
				m.signatureAndHashes[i].hash = d[0]
				m.signatureAndHashes[i].signature = d[1]
				d = d[2:]
			}
		case extensionRenegotiationInfo:
			if length == 0 {
				return false
			}
			d := data[:length]
			l := int(d[0])
			d = d[1:]
			if l != len(d) {
				return false
			}

			m.secureRenegotiation = d
			m.secureRenegotiationSupported = true
		case extensionALPN:
			if length < 2 {
				return false
			}
			l := int(data[0])<<8 | int(data[1])
			if l != length-2 {
				return false
			}
			d := data[2:length]
			for len(d) != 0 {
				stringLen := int(d[0])
				d = d[1:]
				if stringLen == 0 || stringLen > len(d) {
					return false
				}
				m.alpnProtocols = append(m.alpnProtocols, string(d[:stringLen]))
				d = d[stringLen:]
			}
		case extensionSCT:
			m.scts = true
			if length != 0 {
				return false
			}
		}
		data = data[length:]
	}

	return true
}

// 输出日志
func serviceLogger(log string, color int, isDebug bool) {
	if isDebug && !EnableDebug {
		return
	}
	log = strings.Replace(log, "\n", "", -1)
	log = strings.Join([]string{time.Now().Format("2006/01/02 15:04:05"), " ", log}, "")
	if color == 0 {
		fmt.Printf("%s\n", log)
	} else {
		fmt.Printf("%c[1;0;%dm%s%c[0m\n", 0x1B, color, log, 0x1B)
	}
	if LogFilePath != "" {
		fd, _ := os.OpenFile(LogFilePath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
		fdContent := strings.Join([]string{log, "\n"}, "")
		buf := []byte(fdContent)
		fd.Write(buf)
		fd.Close()
	}
}
