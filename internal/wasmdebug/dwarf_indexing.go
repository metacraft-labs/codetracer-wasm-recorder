package wasmdebug

import (
	"debug/dwarf"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/rdleal/intervalst/interval"
)

type LineRecord struct {
	FileName string
	Line     int64
	Column   int64
}

type VariableRecord struct {
	Name string
	Addr uint64
}

type FunctionRecord struct {
	Name     string
	FileName string
	Line     int64
	// TODO: params
	Params []VariableRecord
}

type InlineRecord struct {
	Name         string
	FileName     string
	Line         int64
	CallFileName string
	CallLine     int64
	CallColumn   int64
}

type PCRecord struct {
	Line            *interval.SearchTree[LineRecord, uint64]
	Function        *interval.SearchTree[FunctionRecord, uint64]
	InlinedRoutines *interval.SearchTree[[]InlineRecord, uint64]
	Locals          *interval.SearchTree[[]VariableRecord, uint64]
}

func IndexDwarfData(d *dwarf.Data) (ret PCRecord, err error) {
	cmpFn := func(x, y uint64) int {
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	}

	ret = PCRecord{
		Line:            interval.NewSearchTree[LineRecord](cmpFn),
		Function:        interval.NewSearchTree[FunctionRecord](cmpFn),
		InlinedRoutines: interval.NewSearchTree[[]InlineRecord](cmpFn),
		Locals:          interval.NewSearchTree[[]VariableRecord](cmpFn),
	}

	r := d.Reader()

	for {
		ent, err := r.Next()
		if err != nil || ent == nil {
			break
		}

		switch ent.Tag {
		case dwarf.TagCompileUnit, dwarf.TagInlinedSubroutine, dwarf.TagSubprogram:
		default:
			// Only CompileUnit, InlinedSubroutines ans Subprograms are relevant.
			continue
		}

		// Check if the entry spans the range which contains the target instruction.
		ranges, err := d.Ranges(ent)
		if err != nil {
			continue
		}

		for _, pcs := range ranges {
			start, end := pcs[0], pcs[1]

			if isTombstoneAddr(start) || isTombstoneAddr(end) {
				continue
			}

			switch ent.Tag {
			case dwarf.TagCompileUnit:
				if err := indexCompileUnit(ent, d, &ret); err != nil {
					continue
				}
			case dwarf.TagInlinedSubroutine:

			case dwarf.TagSubprogram:
			}
		}
	}

	return
}

func indexCompileUnit(cu *dwarf.Entry, d *dwarf.Data, tree *PCRecord) error {
	lineReader, err := d.LineReader(cu)
	if err != nil || lineReader == nil {
		return fmt.Errorf("can't initialize line reader: %v", err)
	}

	var le dwarf.LineEntry
	// Get the lines inside the entry.
	lines := make([]dwarf.LineEntry, 0)
	for {
		err = lineReader.Next(&le)
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		// TODO: Maybe we should ignore tombstone addresses by using isTombstoneAddr,
		//  but not sure if that would be an issue in practice.
		lines = append(lines, le)
	}

	sort.Slice(lines, func(i, j int) bool { return lines[i].Address < lines[j].Address })

	// start := 0
	// for i, le := range lines {
	// 	if le.File != lines[start].File || le.Line != lines[start].Line || le.Column != lines[start].Column {
	// 		fmt.Printf("BLQBLQ %v:%v:%v (%v <-> %v)-> %v %v\n", lines[start].File.Name, lines[start].Line, lines[start].Column, start, i-1, lines[start].Address, lines[i-1].Address)
	// 		tree.Line.Insert(lines[start].Address, lines[i-1].Address, LineRecord{
	// 			FileName: lines[start].File.Name,
	// 			Line:     int64(lines[start].Line),
	// 			Column:   int64(lines[start].Column),
	// 		})
	// 		start = i
	// 	}
	// }

	for i, _ := range lines {
		if i-1 >= 0 {
			// fmt.Printf("BLQBLQ %v:%v:%v (%v <-> %v)-> %v %v\n", lines[i].File.Name, lines[i].Line, lines[i].Column, i-1, i, lines[i-1].Address, lines[i].Address-1)
			tree.Line.Insert(lines[i-1].Address, lines[i].Address-1, LineRecord{
				FileName: lines[i].File.Name,
				Line:     int64(lines[i].Line),
				Column:   int64(lines[i].Column),
			})
		}
	}

	return nil
}
