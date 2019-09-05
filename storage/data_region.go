package storage

import (
	"fmt"
	"io"

	"github.com/pkg/errors"
	"github.com/thesues/cannyls-go/block"
	"github.com/thesues/cannyls-go/internalerror"
	"github.com/thesues/cannyls-go/lump"
	"github.com/thesues/cannyls-go/nvm"
	"github.com/thesues/cannyls-go/portion"
	"github.com/thesues/cannyls-go/storage/allocator"
	"github.com/thesues/cannyls-go/util"
)

const (
	LUMP_DATA_TRAILER_SIZE = 2
)

type DataRegion struct {
	allocator  allocator.DataPortionAlloc
	nvm        nvm.NonVolatileMemory
	block_size block.BlockSize
}

func NewDataRegion(alloc allocator.DataPortionAlloc, nvm nvm.NonVolatileMemory) *DataRegion {
	return &DataRegion{
		allocator:  alloc,
		nvm:        nvm,
		block_size: block.Min(),
	}
}

func (region *DataRegion) shiftBlockSize(size uint32) uint32 {
	local_size := uint32(region.block_size.AsU16())
	return (size + uint32(local_size) - 1) / local_size

}

/*
* data region format on disk
*        0                   1                   2                   3
       0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
      +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
      |                         Lump Data (Variable)
      +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
      |                         Padding (Variable)
      +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
      |         Padding size          |
      +-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
*/

//WARNING: this PUT would CHANGE (data *lump.LumpData),
func (region *DataRegion) Put(data lump.LumpData) (portion.DataPortion, error) {
	//
	size := data.Inner.Len() + LUMP_DATA_TRAILER_SIZE

	//Aligned
	data.Inner.AlignResize(size)

	trailer_offset := data.Inner.Len() - LUMP_DATA_TRAILER_SIZE
	padding_len := data.Inner.Len() - size

	if padding_len >= uint32(data.Inner.BlockSize().AsU16()) {
		panic("data region put's align is wrong")
	}
	util.PutUINT16(data.Inner.AsBytes()[trailer_offset:], uint16(padding_len))

	required_blocks := region.shiftBlockSize(data.Inner.Len())
	data_portion, err := region.allocator.Allocate(uint16(required_blocks))

	if err != nil {
		return portion.DataPortion{}, err
	}

	offset, len := data_portion.ShiftBlockToBytes(region.block_size)
	if len != data.Inner.Len() {
		panic(fmt.Sprintf("should be the same in data_region put userdata:%d , diskdata:%d",
			data.Inner.Len(), len))
		//FIXME
	}
	if _, err = region.nvm.Seek(int64(offset), io.SeekStart); err != nil {
		return data_portion, err
	}
	if _, err = region.nvm.Write(data.Inner.AsBytes()); err != nil {
		return data_portion, err
	}

	return data_portion, err
}

func (region *DataRegion) readBlocks(readOffset int64, blockCount int) ([]byte, error) {
	ab := block.NewAlignedBytes(blockCount*int(region.block_size), region.block_size)
	ab.Align()
	_, err := region.nvm.Seek(readOffset, io.SeekStart)
	if err != nil {
		return nil, err
	}
	_, err = region.nvm.Read(ab.AsBytes())
	if err != nil {
		return nil, err
	}
	return ab.AsBytes(), nil
}

func (region *DataRegion) Update(dataPortion portion.DataPortion,
	startOffset uint32, payload []byte) error {

	offsetToDisk, onDiskSize := dataPortion.ShiftBlockToBytes(region.block_size)
	if startOffset+uint32(len(payload)) > onDiskSize-LUMP_DATA_TRAILER_SIZE {
		return errors.Wrap(internalerror.InvalidInput,
			"object reserved capacity exceeded")
	}
	readOffset := region.block_size.FloorAlign(offsetToDisk + uint64(startOffset))
	data, err := region.readBlocks(int64(readOffset),
		(len(payload)+int(region.block_size)-1)/int(region.block_size))
	if err != nil {
		return err
	}
	prefixPadding := startOffset % region.block_size.AsU32()
	copy(data[prefixPadding:], payload)
	if startOffset+uint32(len(payload)) > onDiskSize-region.block_size.AsU32() {
		paddingSize := onDiskSize -
			startOffset - uint32(len(payload)) - LUMP_DATA_TRAILER_SIZE
		originalPaddingSize := util.GetUINT16(data[len(data)-2:])
		if paddingSize < uint32(originalPaddingSize) {
			return errors.Wrap(internalerror.InvalidInput,
				"object reserved capacity exceeded")
		}
	}

	_, err = region.nvm.Seek(int64(readOffset), io.SeekStart)
	if err != nil {
		return err
	}
	// data is already aligned since it's returned from AlignedBytes
	_, err = region.nvm.Write(data)
	return err
}

func (region *DataRegion) Release(portion portion.DataPortion) {
	region.allocator.Release(portion)
}

func (region *DataRegion) Get(portion portion.DataPortion) (lump.LumpData, error) {
	offset, len := portion.ShiftBlockToBytes(region.block_size)

	if _, err := region.nvm.Seek(int64(offset), io.SeekStart); err != nil {
		return lump.LumpData{}, err
	}

	ab := block.NewAlignedBytes(int(len), region.block_size)

	if _, err := region.nvm.Read(ab.AsBytes()); err != nil {
		return lump.LumpData{}, err
	}

	padding_size := uint32(util.GetUINT16(ab.AsBytes()[ab.Len()-2:]))

	ab.Resize(ab.Len() - padding_size - LUMP_DATA_TRAILER_SIZE)
	return lump.NewLumpDataWithAb(ab), nil
}

//more friendly data portion read. only read up user required data.
//the returned bytes could be less than length
func (region *DataRegion) GetWithOffset(portion portion.DataPortion, startOffset uint32, length uint32) ([]byte, error) {

	offset, onDiskSize := portion.ShiftBlockToBytes(region.block_size)

	if startOffset+length > onDiskSize-LUMP_DATA_TRAILER_SIZE {
		return nil, errors.Wrap(internalerror.InvalidInput, "given length is too big")
	}

	newReadStart := region.block_size.FloorAlign(offset + uint64(startOffset))
	prefixPadding := startOffset % region.block_size.AsU32()

	data, err := region.readBlocks(int64(newReadStart),
		(int(length)+int(region.block_size)-1)/int(region.block_size))
	if err != nil {
		return nil, err
	}
	//If length is small, and the read op doesn't reach the last block
	if startOffset+length <= onDiskSize-region.block_size.AsU32() {
		return data[prefixPadding : prefixPadding+length], nil
	}

	//In this case, if length is too big(reach to the last block), prevent to read the padding data
	padding_size := uint32(util.GetUINT16(data[len(data)-2:]))
	realFileSize := util.Min32(uint32(len(data))-padding_size-LUMP_DATA_TRAILER_SIZE, prefixPadding+length)
	if prefixPadding > realFileSize {
		return nil, errors.Wrap(internalerror.InvalidInput,
			"startOffset > object length")
	}
	return data[prefixPadding:realFileSize], nil
}
