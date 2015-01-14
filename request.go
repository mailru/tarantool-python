package tarantool

import (
	"errors"
	"gopkg.in/vmihailenco/msgpack.v2"
	"time"
)

type Request struct {
	conn        *Connection
	requestId   uint32
	requestCode int32
	body        map[int]interface{}
}

type Future struct {
	conn *Connection
	id   uint32
	r    responseAndError
	t    *time.Timer
	tc   <-chan time.Time
}

func (conn *Connection) NewRequest(requestCode int32) (req *Request) {
	req = &Request{}
	req.conn = conn
	req.requestId = conn.nextRequestId()
	req.requestCode = requestCode
	req.body = make(map[int]interface{})

	return
}

func (conn *Connection) Ping() (resp *Response, err error) {
	request := conn.NewRequest(PingRequest)
	resp, err = request.perform()
	return
}

func (r *Request) fillSearch(spaceNo, indexNo uint32, key []interface{}) {
	r.body[KeySpaceNo] = spaceNo
	r.body[KeyIndexNo] = indexNo
	r.body[KeyKey] = key
}
func (r *Request) fillIterator(offset, limit, iterator uint32) {
	r.body[KeyIterator] = iterator
	r.body[KeyOffset] = offset
	r.body[KeyLimit] = limit
}

func (r *Request) fillInsert(spaceNo uint32, tuple []interface{}) {
	r.body[KeySpaceNo] = spaceNo
	r.body[KeyTuple] = tuple
}

func (conn *Connection) Select(spaceNo, indexNo, offset, limit, iterator uint32, key []interface{}) (resp *Response, err error) {
	request := conn.NewRequest(SelectRequest)
	request.fillSearch(spaceNo, indexNo, key)
	request.fillIterator(offset, limit, iterator)
	resp, err = request.perform()
	return
}

func (conn *Connection) SelectTyped(spaceNo, indexNo, offset, limit, iterator uint32, key []interface{}, result interface{}) error {
	request := conn.NewRequest(SelectRequest)
	request.fillSearch(spaceNo, indexNo, key)
	request.fillIterator(offset, limit, iterator)
	return request.performTyped(result)
}

func (conn *Connection) Insert(spaceNo uint32, tuple []interface{}) (resp *Response, err error) {
	request := conn.NewRequest(InsertRequest)
	request.fillInsert(spaceNo, tuple)
	resp, err = request.perform()
	return
}

func (conn *Connection) Replace(spaceNo uint32, tuple []interface{}) (resp *Response, err error) {
	request := conn.NewRequest(ReplaceRequest)
	request.fillInsert(spaceNo, tuple)
	resp, err = request.perform()
	return
}

func (conn *Connection) Delete(spaceNo, indexNo uint32, key []interface{}) (resp *Response, err error) {
	request := conn.NewRequest(DeleteRequest)
	request.fillSearch(spaceNo, indexNo, key)
	resp, err = request.perform()
	return
}

func (conn *Connection) Update(spaceNo, indexNo uint32, key, tuple []interface{}) (resp *Response, err error) {
	request := conn.NewRequest(UpdateRequest)
	request.fillSearch(spaceNo, indexNo, key)
	request.body[KeyTuple] = tuple
	resp, err = request.perform()
	return
}

func (conn *Connection) Call(functionName string, tuple []interface{}) (resp *Response, err error) {
	request := conn.NewRequest(CallRequest)
	request.body[KeyFunctionName] = functionName
	request.body[KeyTuple] = tuple
	resp, err = request.perform()
	return
}

func (conn *Connection) SelectAsync(spaceNo, indexNo, offset, limit, iterator uint32, key []interface{}) *Future {
	request := conn.NewRequest(SelectRequest)
	request.fillSearch(spaceNo, indexNo, key)
	request.fillIterator(offset, limit, iterator)
	return request.future()
}

func (conn *Connection) InsertAsync(spaceNo uint32, tuple []interface{}) *Future {
	request := conn.NewRequest(InsertRequest)
	request.fillInsert(spaceNo, tuple)
	return request.future()
}

func (conn *Connection) ReplaceAsync(spaceNo uint32, tuple []interface{}) *Future {
	request := conn.NewRequest(ReplaceRequest)
	request.fillInsert(spaceNo, tuple)
	return request.future()
}

func (conn *Connection) DeleteAsync(spaceNo, indexNo uint32, key []interface{}) *Future {
	request := conn.NewRequest(DeleteRequest)
	request.fillSearch(spaceNo, indexNo, key)
	return request.future()
}

func (conn *Connection) UpdateAsync(spaceNo, indexNo uint32, key, tuple []interface{}) *Future {
	request := conn.NewRequest(UpdateRequest)
	request.fillSearch(spaceNo, indexNo, key)
	request.body[KeyTuple] = tuple
	return request.future()
}

func (conn *Connection) CallAsync(functionName string, tuple []interface{}) *Future {
	request := conn.NewRequest(CallRequest)
	request.body[KeyFunctionName] = functionName
	request.body[KeyTuple] = tuple
	return request.future()
}

//
// To be implemented
//
func (conn *Connection) Auth(key, tuple []interface{}) (resp *Response, err error) {
	return
}

//
// private
//

func (req *Request) perform() (resp *Response, err error) {
	return req.future().Get()
}

func (req *Request) performTyped(res interface{}) (err error) {
	return req.future().GetTyped(res)
}

func (req *Request) pack() (packet []byte, err error) {
	var body []byte
	rid := req.requestId
	h := [...]byte{
		0xce, 0, 0, 0, 0, // length
		0x82,                           // 2 element map
		KeyCode, byte(req.requestCode), // request code
		KeySync, 0xce,
		byte(rid >> 24), byte(rid >> 16),
		byte(rid >> 8), byte(rid),
	}

	body, err = msgpack.Marshal(req.body)
	if err != nil {
		return
	}

	l := uint32(len(h) - 5 + len(body))
	h[1] = byte(l >> 24)
	h[2] = byte(l >> 16)
	h[3] = byte(l >> 8)
	h[4] = byte(l)

	packet = append(h[:], body...)
	return
}

func (req *Request) future() (f *Future) {
	f = &Future{
		conn: req.conn,
		id:   req.requestId,
		r:    responseAndError{c: make(chan struct{})},
	}
	var packet []byte
	if packet, f.r.r.Error = req.pack(); f.r.r.Error != nil {
		close(f.r.c)
		return
	}

	req.conn.mutex.Lock()
	if req.conn.closed {
		req.conn.mutex.Unlock()
		f.r.r.Error = errors.New("using closed connection")
		close(f.r.c)
		return
	}
	req.conn.requests[req.requestId] = &f.r
	req.conn.mutex.Unlock()
	req.conn.packets <- (packet)

	if req.conn.opts.Timeout > 0 {
		f.t = time.NewTimer(req.conn.opts.Timeout)
		f.tc = f.t.C
	}
	return
}

func (f *Future) wait() {
	select {
	case <-f.r.c:
	default:
		select {
		case <-f.r.c:
		case <-f.tc:
			f.conn.mutex.Lock()
			delete(f.conn.requests, f.id)
			f.conn.mutex.Unlock()
			f.r.r.Error = errors.New("client timeout")
			close(f.r.c)
		}
	}
	if f.t != nil {
		f.t.Stop()
		f.t = nil
		f.tc = nil
	}
}

func (f *Future) Get() (*Response, error) {
	f.wait()
	return f.r.get()
}

func (f *Future) GetTyped(r interface{}) error {
	f.wait()
	return f.r.getTyped(r)
}
