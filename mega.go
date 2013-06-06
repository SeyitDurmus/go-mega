package mega

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/big"
	mrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Default settings
const (
	API_URL              = "https://eu.api.mega.co.nz/cs"
	RETRIES              = 5
	DOWNLOAD_WORKERS     = 3
	MAX_DOWNLOAD_WORKERS = 6
	UPLOAD_WORKERS       = 1
	MAX_UPLOAD_WORKERS   = 6
	TIMEOUT              = time.Second * 10
)

type config struct {
	baseurl    string
	retries    int
	dl_workers int
	ul_workers int
	timeout    time.Duration
}

func newConfig() config {
	return config{
		baseurl:    API_URL,
		retries:    RETRIES,
		dl_workers: DOWNLOAD_WORKERS,
		ul_workers: UPLOAD_WORKERS,
		timeout:    TIMEOUT,
	}
}

// Set mega service base url
func (c *config) SetAPIUrl(u string) {
	c.baseurl = u
}

// Set number of retries for api calls
func (c *config) SetRetries(r int) {
	c.retries = r
}

// Set concurrent download workers
func (c *config) SetDownloadWorkers(w int) error {
	if w <= MAX_DOWNLOAD_WORKERS {
		c.dl_workers = w
		return nil
	}

	return EWORKER_LIMIT_EXCEEDED
}

// Set connection timeout
func (c *config) SetTimeOut(t time.Duration) {
	c.timeout = t
}

// Set concurrent upload workers
func (c *config) SetUploadWorkers(w int) error {
	if w <= MAX_UPLOAD_WORKERS {
		c.ul_workers = w
		return nil
	}

	return EWORKER_LIMIT_EXCEEDED
}

type Mega struct {
	config
	// Sequence number
	sn int64
	// Session ID
	sid []byte
	// Master key
	k []byte
	// User handle
	uh []byte
	// Filesystem object
	FS *MegaFS
}

// Filesystem node types
const (
	FILE   = 0
	FOLDER = 1
	ROOT   = 2
	INBOX  = 3
	TRASH  = 4
)

// Filesystem node
type Node struct {
	name     string
	hash     string
	parent   *Node
	children []*Node
	ntype    int
	size     int64
	ts       time.Time
	meta     NodeMeta
}

func (n *Node) RemoveChild(c *Node) bool {
	index := -1
	for i, v := range n.children {
		if v == c {
			index = i
			break
		}
	}

	if index >= 0 {
		n.children[index] = n.children[len(n.children)-1]
		n.children = n.children[:len(n.children)-1]
		return true
	}

	return false
}

func (n *Node) AddChild(c *Node) {
	if n != nil {
		n.children = append(n.children, c)
	}
}

func (n Node) GetChildren() []*Node {
	return n.children
}

func (n Node) GetType() int {
	return n.ntype
}

func (n Node) GetSize() int64 {
	return n.size
}

func (n Node) GetTimeStamp() time.Time {
	return n.ts
}

func (n Node) GetName() string {
	return n.name
}

type NodeMeta struct {
	key     []byte
	compkey []byte
	iv      []byte
	mac     []byte
}

// Mega filesystem object
type MegaFS struct {
	root   *Node
	trash  *Node
	inbox  *Node
	sroots []*Node
	lookup map[string]*Node
	skmap  map[string]string
}

// Get filesystem root node
func (fs MegaFS) GetRoot() *Node {
	return fs.root
}

// Get filesystem trash node
func (fs MegaFS) GetTrash() *Node {
	return fs.trash
}

// Get inbox node
func (fs MegaFS) GetInbox() *Node {
	return fs.inbox
}

// Get a node pointer from its hash
func (fs MegaFS) HashLookup(h string) *Node {
	if node, ok := fs.lookup[h]; ok {
		return node
	}

	return nil
}

// Retreive all the nodes in the given node tree path by name
// This method returns array of nodes upto the matched subpath
// (in same order as input names array) even if the target node is not located.
func (fs MegaFS) PathLookup(root *Node, ns []string) ([]*Node, error) {
	if root == nil {
		return nil, EARGS
	}

	var err error
	var found bool = true

	nodepath := []*Node{}

	children := root.children
	for _, name := range ns {
		found = false
		for _, n := range children {
			if n.name == name {
				nodepath = append(nodepath, n)
				children = n.children
				found = true
				break
			}
		}

		if found == false {
			break
		}
	}

	if found == false {
		err = ENOENT
	}

	return nodepath, err
}

// Get top level directory nodes shared by other users
func (fs MegaFS) GetSharedRoots() []*Node {
	return fs.sroots
}

func newMegaFS() *MegaFS {
	fs := &MegaFS{
		lookup: make(map[string]*Node),
		skmap:  make(map[string]string),
	}
	return fs
}

func New() *Mega {
	max := big.NewInt(0x100000000)
	bigx, _ := rand.Int(rand.Reader, max)
	cfg := newConfig()
	mgfs := newMegaFS()
	m := &Mega{
		config: cfg,
		sn:     bigx.Int64(),
		FS:     mgfs,
	}
	return m
}

// API request method
func (m *Mega) api_request(r []byte) ([]byte, error) {
	var err error
	var resp *http.Response
	var buf []byte

	defer func() {
		m.sn++
	}()

	url := fmt.Sprintf("%s?id=%d", m.baseurl, m.sn)

	if m.sid != nil {
		url = fmt.Sprintf("%s&sid=%s", url, string(m.sid))
	}

	for i := 0; i < m.retries+1; i++ {
		client := newHttpClient(m.timeout)
		resp, err = client.Post(url, "application/json", bytes.NewBuffer(r))
		if err == nil {
			if resp.StatusCode == 200 {
				goto success
			}
			err = errors.New("Http Status:" + resp.Status)
		}

		if err != nil {
			continue
		}

	success:
		buf, _ = ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if bytes.HasPrefix(buf, []byte("[")) == false {
			return nil, EBADRESP
		}

		if len(buf) < 6 {
			var emsg [1]ErrorMsg
			err = json.Unmarshal(buf, &emsg)
			if err != nil {
				err = json.Unmarshal(buf, &emsg[0])
			}
			if err != nil {
				return buf, EBADRESP
			}
			err = parseError(emsg[0])
			if err == EAGAIN {
				continue
			}

			return buf, err
		}

		if err == nil {
			return buf, nil
		}
	}

	return nil, err
}

// Authenticate and start a session
func (m *Mega) Login(email string, passwd string) error {
	var msg [1]LoginMsg
	var res [1]LoginResp
	var err error
	var result []byte

	passkey := password_key(passwd)
	uhandle := stringhash(email, passkey)
	m.uh = make([]byte, len(uhandle))
	copy(m.uh, uhandle)

	msg[0].Cmd = "us"
	msg[0].User = email
	msg[0].Handle = string(uhandle)

	req, _ := json.Marshal(msg)
	result, err = m.api_request(req)

	if err != nil {
		return err
	}

	err = json.Unmarshal(result, &res)
	if err != nil {
		return err
	}

	m.k = base64urldecode([]byte(res[0].Key))
	cipher, err := aes.NewCipher(passkey)
	cipher.Decrypt(m.k, m.k)
	m.sid = decryptSessionId([]byte(res[0].Privk), []byte(res[0].Csid), m.k)

	return err
}

// Get user information
func (m Mega) GetUser() (UserResp, error) {
	var msg [1]UserMsg
	var res [1]UserResp

	msg[0].Cmd = "ug"

	req, _ := json.Marshal(msg)
	result, err := m.api_request(req)

	if err != nil {
		return res[0], err
	}

	err = json.Unmarshal(result, &res)
	return res[0], err
}

// Add a node into filesystem
func (m *Mega) AddFSNode(itm FSNode) (*Node, error) {
	var compkey, key []uint32
	var attr FileAttr
	var node, parent *Node
	var err error

	master_aes, _ := aes.NewCipher(m.k)

	switch {
	case itm.T == FOLDER || itm.T == FILE:
		args := strings.Split(itm.Key, ":")

		switch {
		// File or folder owned by current user
		case args[0] == itm.User:
			buf := base64urldecode([]byte(args[1]))
			blockDecrypt(master_aes, buf, buf)
			compkey = bytes_to_a32(buf)
			// Shared folder
		case itm.SUser != "" && itm.SKey != "":
			sk := base64urldecode([]byte(itm.SKey))
			blockDecrypt(master_aes, sk, sk)
			sk_aes, _ := aes.NewCipher(sk)

			m.FS.skmap[itm.Hash] = itm.SKey
			buf := base64urldecode([]byte(args[1]))
			blockDecrypt(sk_aes, buf, buf)
			compkey = bytes_to_a32(buf)
			// Shared file
		default:
			k := m.FS.skmap[args[0]]
			b := base64urldecode([]byte(k))
			blockDecrypt(master_aes, b, b)
			block, _ := aes.NewCipher(b)
			buf := base64urldecode([]byte(args[1]))
			blockDecrypt(block, buf, buf)
			compkey = bytes_to_a32(buf)
		}

		switch {
		case itm.T == FILE:
			key = []uint32{compkey[0] ^ compkey[4], compkey[1] ^ compkey[5], compkey[2] ^ compkey[6], compkey[3] ^ compkey[7]}
		default:
			key = compkey
		}

		attr, err = decryptAttr(a32_to_bytes(key), []byte(itm.Attr))
		// FIXME:
		if err != nil {
			attr.Name = "UNKNOWN"
		}
	}

	n, ok := m.FS.lookup[itm.Hash]
	switch {
	case ok:
		node = n
	default:
		node = &Node{
			ntype: itm.T,
			size:  itm.Sz,
			ts:    time.Unix(itm.Ts, 0),
		}

		m.FS.lookup[itm.Hash] = node
	}

	n, ok = m.FS.lookup[itm.Parent]
	switch {
	case ok:
		parent = n
		parent.AddChild(node)
	default:
		parent = nil
		if itm.Parent != "" {
			parent = &Node{
				children: []*Node{node},
				ntype:    FOLDER,
			}
			m.FS.lookup[itm.Parent] = parent
		}
	}

	switch {
	case itm.T == FILE:
		var meta NodeMeta
		meta.key = a32_to_bytes(key)
		meta.iv = a32_to_bytes([]uint32{compkey[4], compkey[5], 0, 0})
		meta.mac = a32_to_bytes([]uint32{compkey[6], compkey[7]})
		meta.compkey = a32_to_bytes(compkey)
		node.meta = meta
	case itm.T == ROOT:
		attr.Name = "Cloud Drive"
		m.FS.root = node
	case itm.T == INBOX:
		attr.Name = "InBox"
		m.FS.inbox = node
	case itm.T == TRASH:
		attr.Name = "Trash"
		m.FS.trash = node
	}

	// Shared directories
	if itm.SUser != "" && itm.SKey != "" {
		m.FS.sroots = append(m.FS.sroots, node)
	}

	node.name = attr.Name
	node.hash = itm.Hash
	node.parent = parent
	node.ntype = itm.T

	return node, nil
}

// Get all nodes from filesystem
func (m *Mega) GetFileSystem() error {
	var msg [1]FilesMsg
	var res [1]FilesResp

	msg[0].Cmd = "f"
	msg[0].C = 1

	req, _ := json.Marshal(msg)
	result, err := m.api_request(req)

	if err != nil {
		return err
	}

	err = json.Unmarshal(result, &res)
	if err != nil {
		return err
	}

	for _, sk := range res[0].Ok {
		m.FS.skmap[sk.Hash] = sk.Key
	}

	for _, itm := range res[0].F {
		m.AddFSNode(itm)
	}

	return nil
}

// Download file from filesystem
func (m Mega) DownloadFile(src *Node, dstpath string, progress *chan int) error {
	defer func() {
		if progress != nil {
			close(*progress)
		}
	}()

	if src == nil {
		return EARGS
	}

	var msg [1]DownloadMsg
	var res [1]DownloadResp
	var outfile *os.File
	var mutex sync.Mutex

	_, err := os.Stat(dstpath)
	if os.IsExist(err) {
		os.Remove(dstpath)
	}

	outfile, err = os.OpenFile(dstpath, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}

	msg[0].Cmd = "g"
	msg[0].G = 1
	msg[0].N = src.hash

	request, _ := json.Marshal(msg)
	result, err := m.api_request(request)
	if err != nil {
		return err
	}

	err = json.Unmarshal(result, &res)
	if err != nil {
		return err
	}
	resourceUrl := res[0].G

	_, err = decryptAttr(src.meta.key, []byte(res[0].Attr))

	aes_block, _ := aes.NewCipher(src.meta.key)

	mac_data := a32_to_bytes([]uint32{0, 0, 0, 0})
	mac_enc := cipher.NewCBCEncrypter(aes_block, mac_data)
	t := bytes_to_a32(src.meta.iv)
	iv := a32_to_bytes([]uint32{t[0], t[1], t[0], t[1]})

	sorted_chunks := []int{}
	chunks := getChunkSizes(int(res[0].Size))
	chunk_macs := make([][]byte, len(chunks))

	for k, _ := range chunks {
		sorted_chunks = append(sorted_chunks, k)
	}
	sort.Ints(sorted_chunks)

	workch := make(chan int)
	donech := make(chan error)
	quitch := make(chan bool)

	// Fire chunk download workers
	for w := 0; w < m.dl_workers; w++ {
		go func() {
			var id int
			for {
				// Wait for work blocked on channel
				select {
				case <-quitch:
					return
				case id = <-workch:
				}

				var resource *http.Response
				mutex.Lock()
				chk_start := sorted_chunks[id]
				chk_size := chunks[chk_start]
				mutex.Unlock()
				client := newHttpClient(m.timeout)
				chunk_url := fmt.Sprintf("%s/%d-%d", resourceUrl, chk_start, chk_start+chk_size-1)
				for retry := 0; retry < m.retries+1; retry++ {
					resource, err = client.Get(chunk_url)
					if err == nil {
						break
					}
				}

				var ctr_iv []uint32
				var ctr_aes cipher.Stream
				var chunk []byte

				if err == nil {
					ctr_iv = bytes_to_a32(src.meta.iv)
					ctr_iv[2] = uint32(uint64(chk_start) / 0x1000000000)
					ctr_iv[3] = uint32(chk_start / 0x10)
					ctr_aes = cipher.NewCTR(aes_block, a32_to_bytes(ctr_iv))
					chunk, err = ioutil.ReadAll(resource.Body)
				}

				if err != nil {
					donech <- err
					continue
				}
				resource.Body.Close()
				ctr_aes.XORKeyStream(chunk, chunk)
				outfile.WriteAt(chunk, int64(chk_start))

				enc := cipher.NewCBCEncrypter(aes_block, iv)
				i := 0
				block := []byte{}
				chunk = paddnull(chunk, 16)
				for i = 0; i < len(chunk); i += 16 {
					block = chunk[i : i+16]
					enc.CryptBlocks(block, block)
				}

				mutex.Lock()
				chunk_macs[id] = make([]byte, 16)
				copy(chunk_macs[id], block)
				mutex.Unlock()
				donech <- nil

				if progress != nil {
					*progress <- chk_size
				}
			}
		}()
	}

	var status error

	// Place chunk download jobs to chan
	for id := 0; id < len(chunks); {
		select {
		case workch <- id:
			id += 1
		}
		select {
		case status = <-donech:
			if status != nil {
				for w := 0; w < m.ul_workers; w++ {
					quitch <- true
				}
				break
			}
		}
	}

	if status != nil {
		os.Remove(dstpath)
		return status
	}

	for _, v := range chunk_macs {
		mac_enc.CryptBlocks(mac_data, v)
	}

	outfile.Close()
	tmac := bytes_to_a32(mac_data)
	if bytes.Equal(a32_to_bytes([]uint32{tmac[0] ^ tmac[1], tmac[2] ^ tmac[3]}), src.meta.mac) == false {
		return EMACMISMATCH
	}

	return nil
}

// Upload a file to the filesystem
func (m Mega) UploadFile(srcpath string, parent *Node, name string, progress *chan int) (*Node, error) {
	defer func() {
		if progress != nil {
			close(*progress)
		}
	}()

	if parent == nil {
		return nil, EARGS
	}

	var msg [1]UploadMsg
	var res [1]UploadResp
	var cmsg [1]UploadCompleteMsg
	var cres [1]UploadCompleteResp
	var infile *os.File
	var fileSize int64
	var mutex sync.Mutex

	parenthash := parent.hash
	info, err := os.Stat(srcpath)
	if err == nil {
		fileSize = info.Size()
	}

	infile, err = os.OpenFile(srcpath, os.O_RDONLY, 0666)
	if err != nil {
		return nil, err
	}

	msg[0].Cmd = "u"
	msg[0].S = fileSize
	completion_handle := []byte{}

	request, _ := json.Marshal(msg)
	result, err := m.api_request(request)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(result, &res)
	if err != nil {
		return nil, err
	}

	uploadUrl := res[0].P
	ukey := []uint32{0, 0, 0, 0, 0, 0}
	for i, _ := range ukey {
		ukey[i] = uint32(mrand.Int31())

	}

	kbytes := a32_to_bytes(ukey[:4])
	kiv := a32_to_bytes([]uint32{ukey[4], ukey[5], 0, 0})
	aes_block, _ := aes.NewCipher(kbytes)

	mac_data := a32_to_bytes([]uint32{0, 0, 0, 0})
	mac_enc := cipher.NewCBCEncrypter(aes_block, mac_data)
	iv := a32_to_bytes([]uint32{ukey[4], ukey[5], ukey[4], ukey[5]})

	sorted_chunks := []int{}
	chunks := getChunkSizes(int(fileSize))
	chunk_macs := make([][]byte, len(chunks))

	for k, _ := range chunks {
		sorted_chunks = append(sorted_chunks, k)
	}
	sort.Ints(sorted_chunks)
	workch := make(chan int)
	donech := make(chan error)
	quitch := make(chan bool)

	for w := 0; w < m.ul_workers; w++ {
		go func() {
			var id int
			for {
				select {
				case <-quitch:
					return
				case id = <-workch:
				}

				mutex.Lock()
				chk_start := sorted_chunks[id]
				chk_size := chunks[chk_start]
				mutex.Unlock()
				ctr_iv := bytes_to_a32(kiv)
				ctr_iv[2] = uint32(uint64(chk_start) / 0x1000000000)
				ctr_iv[3] = uint32(chk_start / 0x10)
				ctr_aes := cipher.NewCTR(aes_block, a32_to_bytes(ctr_iv))

				chunk := make([]byte, chk_size)
				n, _ := infile.ReadAt(chunk, int64(chk_start))
				chunk = chunk[:n]

				enc := cipher.NewCBCEncrypter(aes_block, iv)

				i := 0
				block := make([]byte, 16)
				paddedchunk := paddnull(chunk, 16)
				for i = 0; i < len(paddedchunk); i += 16 {
					copy(block[0:16], paddedchunk[i:i+16])
					enc.CryptBlocks(block, block)
				}

				mutex.Lock()
				chunk_macs[id] = make([]byte, 16)
				copy(chunk_macs[id], block)
				mutex.Unlock()

				ctr_aes.XORKeyStream(chunk, chunk)
				client := newHttpClient(m.timeout)
				chk_url := fmt.Sprintf("%s/%d", uploadUrl, chk_start)
				reader := bytes.NewBuffer(chunk)
				req, _ := http.NewRequest("POST", chk_url, reader)
				rsp, err := client.Do(req)
				chunk_resp := []byte{}
				if err == nil {
					chunk_resp, err = ioutil.ReadAll(rsp.Body)
				}

				if err != nil {
					donech <- err
					continue
				}
				rsp.Body.Close()
				if bytes.Equal(chunk_resp, nil) == false {
					mutex.Lock()
					completion_handle = chunk_resp
					mutex.Unlock()

				}
				donech <- nil
				if progress != nil {
					*progress <- chk_size
				}
			}
		}()
	}

	var status error

	// Place chunk upload jobs to chan
	for id := 0; id < len(chunks); {
		select {
		case workch <- id:
			id += 1
		}

		select {
		case status = <-donech:
			if status != nil {
				for w := 0; w < m.ul_workers; w++ {
					quitch <- true
				}
				break
			}
		}
	}

	if status != nil {
		return nil, status
	}

	for _, v := range chunk_macs {
		mac_enc.CryptBlocks(mac_data, v)
	}

	t := bytes_to_a32(mac_data)
	meta_mac := []uint32{t[0] ^ t[1], t[2] ^ t[3]}

	filename := filepath.Base(srcpath)
	if name != "" {
		filename = name
	}
	attr := FileAttr{filename}

	attr_data, _ := encryptAttr(kbytes, attr)

	key := []uint32{ukey[0] ^ ukey[4], ukey[1] ^ ukey[5],
		ukey[2] ^ meta_mac[0], ukey[3] ^ meta_mac[1],
		ukey[4], ukey[5], meta_mac[0], meta_mac[1]}

	buf := a32_to_bytes(key)
	master_aes, _ := aes.NewCipher(m.k)
	iv = a32_to_bytes([]uint32{0, 0, 0, 0})
	enc := cipher.NewCBCEncrypter(master_aes, iv)
	enc.CryptBlocks(buf[:16], buf[:16])
	enc = cipher.NewCBCEncrypter(master_aes, iv)
	enc.CryptBlocks(buf[16:], buf[16:])

	cmsg[0].Cmd = "p"
	cmsg[0].T = parenthash
	cmsg[0].N[0].H = string(completion_handle)
	cmsg[0].N[0].T = FILE
	cmsg[0].N[0].A = string(attr_data)
	cmsg[0].N[0].K = string(base64urlencode(buf))

	request, _ = json.Marshal(cmsg)
	result, err = m.api_request(request)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(result, &cres)
	if err != nil {
		return nil, err
	}
	node, err := m.AddFSNode(cres[0].F[0])

	return node, err
}

// Move a file from one location to another
func (m Mega) Move(src *Node, parent *Node) error {
	if src == nil || parent == nil {
		return EARGS
	}
	var msg [1]MoveFileMsg

	msg[0].Cmd = "m"
	msg[0].N = src.hash
	msg[0].T = parent.hash
	msg[0].I = randString(10)

	request, _ := json.Marshal(msg)
	_, err := m.api_request(request)

	if err != nil {
		return err
	}

	if node, ok := m.FS.lookup[src.parent.hash]; ok {
		node.RemoveChild(node)
		parent.AddChild(src)
		src.parent = parent
	}

	return nil
}

// Rename a file or folder
func (m Mega) Rename(src *Node, name string) error {
	if src == nil {
		return EARGS
	}
	var msg [1]FileAttrMsg

	master_aes, _ := aes.NewCipher(m.k)
	attr := FileAttr{name}
	attr_data, _ := encryptAttr(src.meta.key, attr)
	key := make([]byte, len(src.meta.compkey))
	blockEncrypt(master_aes, key, src.meta.compkey)

	msg[0].Cmd = "a"
	msg[0].Attr = string(attr_data)
	msg[0].Key = string(base64urlencode(key))
	msg[0].N = src.hash
	msg[0].I = randString(10)

	req, _ := json.Marshal(msg)
	_, err := m.api_request(req)

	return err
}

// Create a directory in the filesystem
func (m Mega) CreateDir(name string, parent *Node) (*Node, error) {
	if parent == nil {
		return nil, EARGS
	}
	var msg [1]UploadCompleteMsg
	var res [1]UploadCompleteResp

	compkey := []uint32{0, 0, 0, 0, 0, 0}
	for i, _ := range compkey {
		compkey[i] = uint32(mrand.Int31())
	}

	master_aes, _ := aes.NewCipher(m.k)
	attr := FileAttr{name}
	ukey := a32_to_bytes(compkey[:4])
	attr_data, _ := encryptAttr(ukey, attr)
	key := make([]byte, len(ukey))
	blockEncrypt(master_aes, key, ukey)

	msg[0].Cmd = "p"
	msg[0].T = parent.hash
	msg[0].N[0].H = "xxxxxxxx"
	msg[0].N[0].T = FOLDER
	msg[0].N[0].A = string(attr_data)
	msg[0].N[0].K = string(base64urlencode(key))
	msg[0].I = randString(10)

	req, _ := json.Marshal(msg)
	result, err := m.api_request(req)

	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(result, &res)
	if err != nil {
		return nil, err
	}
	node, err := m.AddFSNode(res[0].F[0])

	return node, err
}

// Delete a file or directory from filesystem
func (m Mega) Delete(node *Node, destroy bool) error {
	if node == nil {
		return EARGS
	}
	if destroy == false {
		m.Move(node, m.FS.trash)
		return nil
	}

	var msg [1]FileDeleteMsg
	msg[0].Cmd = "d"
	msg[0].N = node.hash
	msg[0].I = randString(10)

	req, _ := json.Marshal(msg)
	_, err := m.api_request(req)

	parent := m.FS.lookup[node.hash]
	parent.RemoveChild(node)
	delete(m.FS.lookup, node.hash)

	return err
}
