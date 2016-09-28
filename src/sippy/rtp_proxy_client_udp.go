// Copyright (c) 2003-2005 Maxim Sobolev. All rights reserved.
// Copyright (c) 2006-2014 Sippy Software, Inc. All rights reserved.
// Copyright (c) 2016 Andriy Pylypenko. All rights reserved.
//
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without modification,
// are permitted provided that the following conditions are met:
//
// 1. Redistributions of source code must retain the above copyright notice, this
// list of conditions and the following disclaimer.
//
// 2. Redistributions in binary form must reproduce the above copyright notice,
// this list of conditions and the following disclaimer in the documentation and/or
// other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND
// ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED
// WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
// DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR
// ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES
// (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES;
// LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON
// ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS
// SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
package sippy

import (
    "crypto/rand"
    "fmt"
    "net"
    "time"
    "strings"
    "sync"

    "sippy/conf"
    "sippy/math"
    "sippy/time"
    "sippy/utils"
)

type Rtp_proxy_client_udp struct {
    address             net.Addr
    uopts               *udpServerOpts
    pending_requests    map[string]*rtpp_req_udp
    global_config       sippy_conf.Config
    delay_flt           sippy_math.RecFilter
    worker              *udpServer
    host                string
    port                string
    lock                sync.Mutex
    owner               *Rtp_proxy_client_base
}

type rtpp_req_udp struct {
    next_retr       float64
    triesleft       int64
    timer           *Timeout
    command         string
    result_callback func(string)
    stime           *sippy_time.MonoTime
    retransmits     int
}

func new_rtpp_req_udp(next_retr float64, triesleft int64, timer *Timeout, command string, result_callback func(string)) *rtpp_req_udp {
    stime, _ := sippy_time.NewMonoTime()
    return &rtpp_req_udp{
        next_retr       : next_retr,
        triesleft       : triesleft,
        timer           : timer,
        command         : command,
        result_callback : result_callback,
        stime           : stime,
        retransmits     : 0,
    }
}

func getnretrans(first_retr, timeout float64) (int64, error) {
    if first_retr < 0 {
        return 0, fmt.Errorf("getnretrans(%f, %f)", first_retr, timeout)
    }
    var n int64 = 0
    for {
        timeout -= first_retr
        if timeout < 0 {
            break
        }
        first_retr *= 2.0
        n += 1
    }
    return n, nil
}

func NewRtp_proxy_client_udp(owner *Rtp_proxy_client_base, global_config sippy_conf.Config, address net.Addr, opts *Rtp_proxy_opts) (rtp_proxy_transport, error) {
    var err error

    self := &Rtp_proxy_client_udp{
        owner               : owner,
        address             : address,
        pending_requests    : make(map[string]*rtpp_req_udp),
        global_config       : global_config,
        delay_flt           : sippy_math.NewRecFilter(0.95, 0.25),
    }
    self.host, self.port, err = net.SplitHostPort(self.address.String())
    if err != nil {
        return nil, err
    }
    self.uopts = NewUdpServerOpts(opts.bind_address(), self.process_reply)
    //self.uopts.ploss_out_rate = self.ploss_out_rate
    //self.uopts.pdelay_out_max = self.pdelay_out_max
    if opts != nil && opts.Nworkers != nil {
        self.uopts.nworkers = *opts.Nworkers
    }
    self.worker, err = NewUdpServer(global_config, self.uopts)
    return self, err
}

func (*Rtp_proxy_client_udp) is_local() bool {
    return false
}

func (self *Rtp_proxy_client_udp) send_command(command string, result_callback func(string)) {
    buf := make([]byte, 16)
    rand.Read(buf)
    cookie := fmt.Sprintf("%x", buf)
    next_retr := self.delay_flt.GetLastval() * 4.0
    exp_time := 3.0
    if command[0] == 'I' {
        exp_time = 10.0
    } else if command[0] == 'G' {
        exp_time = 1.0
    }
    nretr, err := getnretrans(next_retr, exp_time)
    if err != nil {
        self.owner.logger.Debug("getnretrans error: " + err.Error())
        return
    }
    command = cookie + " " + command
    timer := StartTimeout(func() { self.retransmit(cookie) }, nil, time.Duration(next_retr * float64(time.Second)), 1, self.owner.logger)
    preq := new_rtpp_req_udp(next_retr, nretr - 1, timer, command, result_callback)
    self.worker.SendTo([]byte(command), self.host, self.port)
    self.lock.Lock()
    self.pending_requests[cookie] = preq
    self.lock.Unlock()
}

func (self *Rtp_proxy_client_udp) retransmit(cookie string) {
    self.lock.Lock()
    req, ok := self.pending_requests[cookie]
    if ! ok {
        return
    }
    if req.triesleft <= 0 || self.worker == nil {
        delete(self.pending_requests, cookie)
        self.lock.Unlock()
        self.owner.me.GoOffline()
        if req.result_callback != nil {
            req.result_callback("")
        }
        return
    }
    req.next_retr *= 2
    req.retransmits += 1
    req.timer = StartTimeout(func() { self.retransmit(cookie) }, nil, time.Duration(req.next_retr * float64(time.Second)), 1, self.owner.logger)
    req.stime, _ = sippy_time.NewMonoTime()
    self.worker.SendTo([]byte(req.command), self.host, self.port)
    req.triesleft -= 1
    self.lock.Unlock()
}

func (self *Rtp_proxy_client_udp) process_reply(data []byte, address *sippy_conf.HostPort, worker *udpServer, rtime *sippy_time.MonoTime) {
    arr := sippy_utils.FieldsN(string(data), 2)
    if len(arr) != 2 {
        self.owner.logger.Debug("Rtp_proxy_client_udp.process_reply(): invalid response " + string(data))
        return
    }
    cookie, result := arr[0], arr[1]
    self.lock.Lock()
    req, ok := self.pending_requests[cookie]
    self.lock.Unlock()
    if ! ok {
        return
    }
    req.timer.Cancel()
    if req.result_callback != nil {
        req.result_callback(strings.TrimSpace(result))
    }
    if req.retransmits == 0 {
        // When we had to do retransmit it is not possible to figure out whether
        // or not this reply is related to the original request or one of the
        // retransmits. Therefore, using it to estimate delay could easily produce
        // bogus value that is too low or even negative if we cook up retransmit
        // while the original response is already in the queue waiting to be
        // processed. This should not be a big issue since UDP command channel does
        // not work very well if the packet loss goes to more than 30-40%.
        self.delay_flt.Apply(rtime.Sub(req.stime).Seconds())
        //print "Rtp_proxy_client_udp.process_reply(): delay %f" % (rtime - stime)
    }
}
/*
    def reconnect(self, address, bind_address = nil):
        self.address = address
        if bind_address != self.uopts.laddress:
            self.uopts.laddress = bind_address
            self.worker.shutdown()
            self.worker = Udp_server(self.global_config, self.uopts)
            self.delay_flt = recfilter(0.95, 0.25)
*/
func (self *Rtp_proxy_client_udp) shutdown() {
    self.worker.Shutdown()
    self.worker = nil
}
/*
    def get_rtpc_delay(self):
        return self.delay_flt.lastval

class selftest(object):
    def gotreply(self, *args):
        from twisted.internet import reactor
        print args
        reactor.crash()

    def run(self):
        import os
        from twisted.internet import reactor
        global_config = {}
        global_config["my_pid"] = os.getpid()
        rtpc = Rtp_proxy_client_udp(global_config, ("127.0.0.1", 22226), nil)
        os.system("sockstat | grep -w %d" % global_config["my_pid"])
        rtpc.send_command("Ib", self.gotreply)
        reactor.run()
        rtpc.reconnect(("localhost", 22226), ("0.0.0.0", 34222))
        os.system("sockstat | grep -w %d" % global_config["my_pid"])
        rtpc.send_command("V", self.gotreply)
        reactor.run()
        rtpc.reconnect(("localhost", 22226), ("127.0.0.1", 57535))
        os.system("sockstat | grep -w %d" % global_config["my_pid"])
        rtpc.send_command("V", self.gotreply)
        reactor.run()
        rtpc.shutdown()

if __name__ == "__main__":
    selftest().run()
*/
