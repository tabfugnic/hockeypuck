/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package openpgp

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/cmars/conflux"
	"github.com/cmars/conflux/recon"
	"github.com/cmars/conflux/recon/pqptree"
	"github.com/jmoiron/sqlx"
	"io"
	"launchpad.net/hockeypuck/hkp"
	"log"
	"net/http"
	"time"
)

type SksPeer struct {
	*recon.Peer
	Service    *hkp.Service
	KeyChanges KeyChangeChan
}

func NewSksPeer(s *hkp.Service) (*SksPeer, error) {
	reconSettings := recon.NewSettings(Config().Settings.TomlTree)
	pqpTreeSettings := pqptree.NewSettings(reconSettings)
	db, err := sqlx.Connect(Config().Driver(), Config().DSN())
	ptree, err := pqptree.New("sks", db, pqpTreeSettings)
	if err != nil {
		return nil, err
	}
	peer := recon.NewPeer(reconSettings, ptree)
	sksPeer := &SksPeer{Peer: peer, Service: s, KeyChanges: make(KeyChangeChan, reconSettings.SplitThreshold())}
	return sksPeer, nil
}

func (r *SksPeer) Start() {
	go r.HandleRecovery()
	go r.HandleKeyUpdates()
	go r.Peer.Start()
}

func (r *SksPeer) HandleKeyUpdates() {
	for {
		select {
		case keyChange, ok := <-r.KeyChanges:
			if !ok {
				return
			}
			digest, err := hex.DecodeString(keyChange.CurrentMd5)
			if err != nil {
				log.Println("bad digest:", keyChange.CurrentMd5)
				continue
			}
			digest = append(digest, byte(0))
			digestZp := conflux.Zb(conflux.P_SKS, digest)
			hasDigest, err := r.Peer.HasElement(digestZp)
			if keyChange.PreviousMd5 != keyChange.CurrentMd5 || !hasDigest {
				log.Println("Prefix tree: Insert:", hex.EncodeToString(digestZp.Bytes()), keyChange, keyChange.CurrentMd5)
				err := r.Peer.Insert(digestZp)
				if err != nil {
					log.Println(err)
					continue
				}
				if keyChange.PreviousMd5 != "" && keyChange.PreviousMd5 != keyChange.CurrentMd5 {
					prevDigest, err := hex.DecodeString(keyChange.PreviousMd5)
					if err != nil {
						log.Println("bad digest:", keyChange.PreviousMd5)
						continue
					}
					prevDigest = append(prevDigest, byte(0))
					prevDigestZp := conflux.Zb(conflux.P_SKS, prevDigest)
					log.Println("Prefix Tree: Remove:", prevDigestZp)
					err = r.Peer.Remove(prevDigestZp)
					if err != nil {
						log.Println(err)
						continue
					}
				}
			}
		}
	}
}

func (r *SksPeer) HandleRecovery() {
	rcvrChans := make(map[string]chan *recon.Recover)
	defer func() {
		for _, ch := range rcvrChans {
			close(ch)
		}
	}()
	for {
		select {
		case rcvr, ok := <-r.Peer.RecoverChan:
			if !ok {
				return
			}
			// Use remote HKP host:port as peer-unique identifier
			remoteAddr, err := rcvr.HkpAddr()
			if err != nil {
				continue
			}
			// Mux recoveries to per-address channels
			rcvrChan, has := rcvrChans[remoteAddr]
			if !has {
				rcvrChan = make(chan *recon.Recover)
				rcvrChans[remoteAddr] = rcvrChan
				go r.handleRemoteRecovery(rcvr, rcvrChan)
			}
			rcvrChan <- rcvr
		}
	}
}

type workRecoveredReady chan interface{}
type workRecoveredWork chan *conflux.ZSet

func (r *SksPeer) handleRemoteRecovery(rcvr *recon.Recover, rcvrChan chan *recon.Recover) {
	recovered := conflux.NewZSet()
	ready := make(workRecoveredReady)
	work := make(workRecoveredWork)
	defer close(work)
	go r.workRecovered(rcvr, ready, work)
	for {
		select {
		case rcvr, ok := <-rcvrChan:
			if !ok {
				return
			}
			// Aggregate recovered IDs
			recovered.AddSlice(rcvr.RemoteElements)
			log.Println("Recovery from", rcvr.RemoteAddr.String(), ":", recovered.Len(), "pending")
		case _, ok := <-ready:
			// Recovery worker is ready for more
			if !ok {
				return
			}
			work <- recovered
			recovered = conflux.NewZSet()
		}
	}
}

func (r *SksPeer) workRecovered(rcvr *recon.Recover, ready workRecoveredReady, work workRecoveredWork) {
	defer close(ready)
	timer := time.NewTimer(time.Duration(r.Peer.GossipIntervalSecs()) * time.Second)
	defer timer.Stop()
	for {
		select {
		case recovered, ok := <-work:
			if !ok {
				return
			}
			err := r.requestRecovered(rcvr, recovered)
			if err != nil {
				log.Println(err)
			}
			timer.Reset(time.Duration(r.Peer.GossipIntervalSecs()) * time.Second)
		case <-timer.C:
			timer.Stop()
			ready <- new(interface{})
		}
	}
}

func (r *SksPeer) requestRecovered(rcvr *recon.Recover, elements *conflux.ZSet) (err error) {
	var remoteAddr string
	remoteAddr, err = rcvr.HkpAddr()
	if err != nil {
		return
	}
	// Make an sks hashquery request
	hqBuf := bytes.NewBuffer(nil)
	err = recon.WriteInt(hqBuf, elements.Len())
	if err != nil {
		return
	}
	for _, z := range elements.Items() {
		zb := z.Bytes()
		err = recon.WriteInt(hqBuf, len(zb))
		if err != nil {
			return
		}
		_, err = hqBuf.Write(zb)
		if err != nil {
			return
		}
	}
	resp, err := http.Post(fmt.Sprintf("http://%s/pks/hashquery", remoteAddr),
		"sks/hashquery", bytes.NewReader(hqBuf.Bytes()))
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var nkeys, keyLen int
	nkeys, err = recon.ReadInt(resp.Body)
	if err != nil {
		return
	}
	log.Println("Response from server:", nkeys, " keys found")
	for i := 0; i < nkeys; i++ {
		keyLen, err = recon.ReadInt(resp.Body)
		if err != nil {
			return
		}
		keyBuf := bytes.NewBuffer(nil)
		_, err = io.CopyN(keyBuf, resp.Body, int64(keyLen))
		if err != nil {
			return
		}
		log.Println("Key#", i+1, ":", keyLen, "bytes")
		// Merge locally
		recoverKey := hkp.NewRecoverKey()
		recoverKey.Keytext = keyBuf.Bytes()
		recoverKey.RecoverSet = elements
		recoverKey.Source = rcvr.RemoteAddr.String()
		go func() {
			r.Service.Requests <- recoverKey
		}()
		resp := <-recoverKey.Response()
		if resp != nil && resp.Error() != nil {
			log.Println("Error adding key:", resp.Error())
		}
	}
	// Read last two bytes (CRLF, why?), or SKS will complain.
	resp.Body.Read(make([]byte, 2))
	return
}

func (r *SksPeer) Stop() {
	r.Peer.Stop()
}
