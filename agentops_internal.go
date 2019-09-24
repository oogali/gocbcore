package gocbcore

import (
	"encoding/binary"
)

// GetMetaOptions encapsulates the parameters for a GetMetaEx operation.
type GetMetaOptions struct {
	Key            []byte
	CollectionName string
	ScopeName      string
	CollectionId   uint32
}

// GetMetaResult encapsulates the result of a GetMetaEx operation.
type GetMetaResult struct {
	Value    []byte
	Flags    uint32
	Cas      Cas
	Expiry   uint32
	SeqNo    SeqNo
	Datatype uint8
	Deleted  uint32
}

// GetMetaExCallback is invoked upon completion of a GetMetaEx operation.
type GetMetaExCallback func(*GetMetaResult, error)

// GetMetaEx retrieves a document along with some internal Couchbase meta-data.
func (agent *Agent) GetMetaEx(opts GetMetaOptions, cb GetMetaExCallback) (PendingOp, error) {
	handler := func(resp *memdQResponse, req *memdQRequest, err error) {
		if err != nil {
			cb(nil, err)
			return
		}

		if len(resp.Extras) != 21 {
			cb(nil, ErrProtocol)
			return
		}

		deleted := binary.BigEndian.Uint32(resp.Extras[0:])
		flags := binary.BigEndian.Uint32(resp.Extras[4:])
		expTime := binary.BigEndian.Uint32(resp.Extras[8:])
		seqNo := SeqNo(binary.BigEndian.Uint64(resp.Extras[12:]))
		dataType := resp.Extras[20]

		cb(&GetMetaResult{
			Value:    resp.Value,
			Flags:    flags,
			Cas:      Cas(resp.Cas),
			Expiry:   expTime,
			SeqNo:    seqNo,
			Datatype: dataType,
			Deleted:  deleted,
		}, nil)
	}

	extraBuf := make([]byte, 1)
	extraBuf[0] = 2

	req := &memdQRequest{
		memdPacket: memdPacket{
			Magic:        reqMagic,
			Opcode:       cmdGetMeta,
			Datatype:     0,
			Cas:          0,
			Extras:       extraBuf,
			Key:          opts.Key,
			Value:        nil,
			CollectionID: opts.CollectionId,
		},
		Callback:       handler,
		CollectionName: opts.CollectionName,
		ScopeName:      opts.ScopeName,
	}

	return agent.dispatchOp(req)
}

// SetMetaOptions encapsulates the parameters for a SetMetaEx operation.
type SetMetaOptions struct {
	Key            []byte
	Value          []byte
	Extra          []byte
	Datatype       uint8
	Options        uint32
	Flags          uint32
	Expiry         uint32
	Cas            Cas
	RevNo          uint64
	CollectionName string
	ScopeName      string
	CollectionId   uint32
}

// SetMetaResult encapsulates the result of a SetMetaEx operation.
type SetMetaResult struct {
	Cas           Cas
	MutationToken MutationToken
}

// SetMetaExCallback is invoked upon completion of a SetMetaEx operation.
type SetMetaExCallback func(*SetMetaResult, error)

// SetMetaEx stores a document along with setting some internal Couchbase meta-data.
func (agent *Agent) SetMetaEx(opts SetMetaOptions, cb SetMetaExCallback) (PendingOp, error) {
	handler := func(resp *memdQResponse, req *memdQRequest, err error) {
		if err != nil {
			cb(nil, err)
			return
		}

		mutToken := MutationToken{}
		if len(resp.Extras) >= 16 {
			mutToken.VbId = req.Vbucket
			mutToken.VbUuid = VbUuid(binary.BigEndian.Uint64(resp.Extras[0:]))
			mutToken.SeqNo = SeqNo(binary.BigEndian.Uint64(resp.Extras[8:]))
		}

		cb(&SetMetaResult{
			Cas:           Cas(resp.Cas),
			MutationToken: mutToken,
		}, nil)
	}

	extraBuf := make([]byte, 30+len(opts.Extra))
	binary.BigEndian.PutUint32(extraBuf[0:], opts.Flags)
	binary.BigEndian.PutUint32(extraBuf[4:], opts.Expiry)
	binary.BigEndian.PutUint64(extraBuf[8:], uint64(opts.RevNo))
	binary.BigEndian.PutUint64(extraBuf[16:], uint64(opts.Cas))
	binary.BigEndian.PutUint32(extraBuf[24:], opts.Options)
	binary.BigEndian.PutUint16(extraBuf[28:], uint16(len(opts.Extra)))
	copy(extraBuf[30:], opts.Extra)
	req := &memdQRequest{
		memdPacket: memdPacket{
			Magic:        reqMagic,
			Opcode:       cmdSetMeta,
			Datatype:     opts.Datatype,
			Cas:          0,
			Extras:       extraBuf,
			Key:          opts.Key,
			Value:        opts.Value,
			CollectionID: opts.CollectionId,
		},
		Callback:       handler,
		CollectionName: opts.CollectionName,
		ScopeName:      opts.ScopeName,
	}

	return agent.dispatchOp(req)
}

// DeleteMetaOptions encapsulates the parameters for a DeleteMetaEx operation.
type DeleteMetaOptions struct {
	Key            []byte
	Value          []byte
	Extra          []byte
	Datatype       uint8
	Options        uint32
	Flags          uint32
	Expiry         uint32
	Cas            Cas
	RevNo          uint64
	CollectionName string
	ScopeName      string
	CollectionId   uint32
}

// DeleteMetaResult encapsulates the result of a DeleteMetaEx operation.
type DeleteMetaResult struct {
	Cas           Cas
	MutationToken MutationToken
}

// DeleteMetaExCallback is invoked upon completion of a DeleteMetaEx operation.
type DeleteMetaExCallback func(*DeleteMetaResult, error)

// DeleteMetaEx deletes a document along with setting some internal Couchbase meta-data.
func (agent *Agent) DeleteMetaEx(opts DeleteMetaOptions, cb DeleteMetaExCallback) (PendingOp, error) {
	handler := func(resp *memdQResponse, req *memdQRequest, err error) {
		if err != nil {
			cb(nil, err)
			return
		}

		mutToken := MutationToken{}
		if len(resp.Extras) >= 16 {
			mutToken.VbId = req.Vbucket
			mutToken.VbUuid = VbUuid(binary.BigEndian.Uint64(resp.Extras[0:]))
			mutToken.SeqNo = SeqNo(binary.BigEndian.Uint64(resp.Extras[8:]))
		}

		cb(&DeleteMetaResult{
			Cas:           Cas(resp.Cas),
			MutationToken: mutToken,
		}, nil)
	}

	extraBuf := make([]byte, 30+len(opts.Extra))
	binary.BigEndian.PutUint32(extraBuf[0:], opts.Flags)
	binary.BigEndian.PutUint32(extraBuf[4:], opts.Expiry)
	binary.BigEndian.PutUint64(extraBuf[8:], opts.RevNo)
	binary.BigEndian.PutUint64(extraBuf[16:], uint64(opts.Cas))
	binary.BigEndian.PutUint32(extraBuf[24:], opts.Options)
	binary.BigEndian.PutUint16(extraBuf[28:], uint16(len(opts.Extra)))
	copy(extraBuf[30:], opts.Extra)
	req := &memdQRequest{
		memdPacket: memdPacket{
			Magic:        reqMagic,
			Opcode:       cmdDelMeta,
			Datatype:     opts.Datatype,
			Cas:          0,
			Extras:       extraBuf,
			Key:          opts.Key,
			Value:        opts.Value,
			CollectionID: opts.CollectionId,
		},
		Callback:       handler,
		CollectionName: opts.CollectionName,
		ScopeName:      opts.ScopeName,
	}

	return agent.dispatchOp(req)
}
