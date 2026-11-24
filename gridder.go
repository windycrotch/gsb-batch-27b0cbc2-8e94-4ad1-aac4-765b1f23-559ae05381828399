package gridder

import (
	"errors"
	"image/color"
	"io"
	"sync"

	"github.com/fogleman/gg"
	"golang.org/x/image/font"
)

var (
	errNoRows       = errors.New("no rows provided")
	errNoColumns    = errors.New("no columns provided")
	errOutOfBounds  = errors.New("out of bounds")
	errEmptyPNGName = errors.New("empty file name")
)

// New creates a new gridder and sets it up with its configuration
func New(imageConfig ImageConfig, gridConfig GridConfig) (*Gridder, error) {
	rows := gridConfig.GetRows()
	if rows == 0 {
		return nil, errNoRows
	}

	columns := gridConfig.GetColumns()
	if columns == 0 {
		return nil, errNoColumns
	}

	gridder := Gridder{
		imageConfig: imageConfig,
		gridConfig:  gridConfig,
		ctx:         gg.NewContext(imageConfig.GetWidth(), imageConfig.GetHeight()),
	}
	gridder.paintBackground()
	return &gridder, nil
}

// Gridder gridder structure
//
// All access to the shared drawing context is guarded by mu. Export methods
// (EncodePNG/SavePNG) never draw onto the shared context: they take a pixel
// snapshot under mu.RLock and render the grid lines and border onto their own
// private context, so repeated and concurrent exports can never accumulate
// strokes or observe each other's intermediate state.
type Gridder struct {
	mu          sync.RWMutex
	imageConfig ImageConfig
	gridConfig  GridConfig
	ctx         *gg.Context
}

// SavePNG saves to PNG
func (g *Gridder) SavePNG() error {
	name := g.imageConfig.GetName()
	if name == "" {
		return errEmptyPNGName
	}

	// Render onto an isolated snapshot; the shared canvas is left untouched,
	// so a failed export never compromises later exports.
	return gg.SavePNG(name, g.render().Image())
}

// EncodePNG encodes the image as a PNG and writes it to the provided io.Writer.
func (g *Gridder) EncodePNG(w io.Writer) error {
	return g.render().EncodePNG(w)
}

// render builds a private context holding a consistent snapshot of the
// current canvas together with the grid lines and the border. It does not
// mutate the gridder's own context, which keeps every export independent,
// repeatable and safe for concurrent use.
func (g *Gridder) render() *gg.Context {
	g.mu.RLock()
	// NewContextForImage copies the source pixels into a fresh RGBA buffer.
	snapshot := gg.NewContextForImage(g.ctx.Image())
	g.mu.RUnlock()

	// The shared context carries the margin translation applied in
	// paintBackground; the snapshot starts with an identity matrix, so apply
	// the same translation before painting grid lines and the border.
	margin := float64(g.gridConfig.GetMarginWidth())
	snapshot.Translate(margin, margin)
	g.paintGrid(snapshot)
	g.paintBorder(snapshot)
	return snapshot
}

// PaintCell paints Cell
func (g *Gridder) PaintCell(row int, column int, paintColor color.Color) error {
	if err := g.verifyInBounds(row, column); err != nil {
		return err
	}

	cellWidth, cellHeight := g.getCellDimensions()
	paintWidth := cellWidth - g.gridConfig.GetLineStrokeWidth()
	paintHeight := cellHeight - g.gridConfig.GetLineStrokeWidth()

	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawRectangle(row, column, RectangleConfig{Width: paintWidth, Height: paintHeight, Color: paintColor})
}

// DrawRectangle draws a rectangle in a cell
func (g *Gridder) DrawRectangle(row int, column int, rectangleConfigs ...RectangleConfig) error {
	if err := g.verifyInBounds(row, column); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawRectangle(row, column, getFirstRectangleConfig(rectangleConfigs...))
}

func (g *Gridder) drawRectangle(row int, column int, rectangleConfig RectangleConfig) error {
	center := g.getCellCenter(row, column)
	rectangleWidth := rectangleConfig.GetWidth()
	rectangleHeight := rectangleConfig.GetHeight()

	x := center.X - rectangleWidth/2
	y := center.Y - rectangleHeight/2

	g.ctx.Push()
	dashes := rectangleConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.RotateAbout(gg.Radians(rectangleConfig.GetRotate()), center.X, center.Y)
	g.ctx.DrawRectangle(x, y, rectangleWidth, rectangleHeight)
	g.ctx.SetLineWidth(rectangleConfig.GetStrokeWidth())
	g.ctx.SetColor(rectangleConfig.GetColor())
	if rectangleConfig.IsStroke() {
		g.ctx.Stroke()
	} else {
		g.ctx.Fill()
	}
	g.ctx.Pop()
	return nil
}

// DrawCircle draws a circle in a cell
func (g *Gridder) DrawCircle(row int, column int, circleConfigs ...CircleConfig) error {
	if err := g.verifyInBounds(row, column); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawCircle(row, column, getFirstCircleConfig(circleConfigs...))
}

func (g *Gridder) drawCircle(row int, column int, circleConfig CircleConfig) error {
	center := g.getCellCenter(row, column)

	g.ctx.Push()
	dashes := circleConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.DrawPoint(center.X, center.Y, circleConfig.GetRadius())
	g.ctx.SetLineWidth(circleConfig.GetStrokeWidth())
	g.ctx.SetColor(circleConfig.GetColor())
	if circleConfig.IsStroke() {
		g.ctx.Stroke()
	} else {
		g.ctx.Fill()
	}
	g.ctx.Pop()
	return nil
}

// DrawPath draws a path between two cells
func (g *Gridder) DrawPath(row1 int, column1 int, row2 int, column2 int, pathConfigs ...PathConfig) error {
	if err := g.verifyInBounds(row1, column1); err != nil {
		return err
	}

	if err := g.verifyInBounds(row2, column2); err != nil {
		return err
	}

	center1 := g.getCellCenter(row1, column1)
	center2 := g.getCellCenter(row2, column2)
	pathConfig := getFirstPathConfig(pathConfigs...)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.ctx.Push()
	dashes := pathConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.SetColor(pathConfig.GetColor())
	g.ctx.SetLineWidth(pathConfig.GetStrokeWidth())
	g.ctx.DrawLine(center1.X, center1.Y, center2.X, center2.Y)
	g.ctx.Stroke()
	g.ctx.Pop()
	return nil
}

// DrawLine draws a line in a cell
func (g *Gridder) DrawLine(row int, column int, lineConfigs ...LineConfig) error {
	if err := g.verifyInBounds(row, column); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	return g.drawLine(row, column, getFirstLineConfig(lineConfigs...))
}

func (g *Gridder) drawLine(row int, column int, lineConfig LineConfig) error {
	center := g.getCellCenter(row, column)
	length := lineConfig.GetLength()

	x1 := center.X - length/2
	x2 := center.X + length/2
	y := center.Y

	g.ctx.Push()
	dashes := lineConfig.GetDashes()
	if dashes > 0 {
		g.ctx.SetDash(dashes)
	} else {
		g.ctx.SetDash()
	}
	g.ctx.RotateAbout(gg.Radians(lineConfig.GetRotate()), center.X, center.Y)
	g.ctx.DrawLine(x1, y, x2, y)
	g.ctx.SetLineWidth(lineConfig.GetStrokeWidth())
	g.ctx.SetColor(lineConfig.GetColor())
	g.ctx.Stroke()
	g.ctx.Pop()
	return nil
}

// DrawString draws a string in a cell
func (g *Gridder) DrawString(row int, column int, text string, fontFace font.Face, stringConfigs ...StringConfig) error {
	if err := g.verifyInBounds(row, column); err != nil {
		return err
	}

	center := g.getCellCenter(row, column)
	stringConfig := getFirstStringConfig(stringConfigs...)

	g.mu.Lock()
	defer g.mu.Unlock()

	g.ctx.Push()
	g.ctx.SetFontFace(fontFace)
	g.ctx.SetColor(stringConfig.GetColor())
	g.ctx.RotateAbout(gg.Radians(stringConfig.GetRotate()), center.X, center.Y)
	g.ctx.DrawStringAnchored(text, center.X, center.Y, 0.5, 0.35)
	g.ctx.Pop()
	return nil
}

func (g *Gridder) paintBackground() {
	margin := float64(g.gridConfig.GetMarginWidth())
	g.ctx.Translate(margin, margin)
	g.ctx.SetColor(g.gridConfig.GetBackgroundColor())
	g.ctx.Clear()
}

// paintGrid paints the grid lines onto the supplied context. It never touches
// the gridder's shared canvas, so it can be invoked per export on an isolated
// snapshot without accumulating strokes across exports.
func (g *Gridder) paintGrid(ctx *gg.Context) {
	canvasWidth, canvasHeight := g.getGridDimensions()
	cellWidth, cellHeight := g.getCellDimensions()

	columns := float64(g.gridConfig.GetColumns())

	ctx.Push()
	for i := 1.0; i < columns; i++ {
		x := i * cellWidth
		ctx.MoveTo(x, 0)
		ctx.LineTo(x, canvasHeight)
	}

	rows := float64(g.gridConfig.GetRows())
	for i := 1.0; i < rows; i++ {
		y := i * cellHeight
		ctx.MoveTo(0, y)
		ctx.LineTo(canvasWidth, y)
	}

	dashes := g.gridConfig.GetLineDashes()
	if dashes > 0 {
		ctx.SetDash(dashes)
	} else {
		ctx.SetDash()
	}
	ctx.SetColor(g.gridConfig.GetLineColor())
	ctx.SetLineWidth(g.gridConfig.GetLineStrokeWidth())
	ctx.Stroke()
	ctx.Pop()
}

// paintBorder paints the border onto the supplied context. It never touches
// the gridder's shared canvas, so it can be invoked per export on an isolated
// snapshot without accumulating strokes across exports.
func (g *Gridder) paintBorder(ctx *gg.Context) {
	canvasWidth, canvasHeight := g.getGridDimensions()
	cellWidth, cellHeight := g.getCellDimensions()

	columns := float64(g.gridConfig.GetColumns())
	ctx.Push()
	ctx.MoveTo(0, 0)
	ctx.LineTo(0, canvasHeight)
	ctx.MoveTo(cellWidth*columns, 0)
	ctx.LineTo(cellWidth*columns, canvasHeight)

	rows := float64(g.gridConfig.GetRows())
	ctx.MoveTo(0, 0)
	ctx.LineTo(canvasWidth, 0)
	ctx.MoveTo(0, cellHeight*rows)
	ctx.LineTo(canvasWidth, cellHeight*rows)

	dashes := g.gridConfig.GetBorderDashes()
	if dashes > 0 {
		ctx.SetDash(dashes)
	} else {
		ctx.SetDash()
	}
	ctx.SetLineWidth(g.gridConfig.GetBorderStrokeWidth())
	ctx.SetColor(g.gridConfig.GetBorderColor())
	ctx.Stroke()
	ctx.Pop()
}

func (g *Gridder) getCellDimensions() (float64, float64) {
	gridWidth, gridHeight := g.getGridDimensions()
	cellWidth := gridWidth / float64(g.gridConfig.GetColumns())
	cellHeight := gridHeight / float64(g.gridConfig.GetRows())
	return cellWidth, cellHeight
}

func (g *Gridder) getGridDimensions() (float64, float64) {
	imageWidth := g.imageConfig.GetWidth()
	imageHeight := g.imageConfig.GetHeight()

	gridWidth := float64(g.gridConfig.GetWidth(imageWidth))
	gridHeight := float64(g.gridConfig.GetHeight(imageHeight))
	return gridWidth, gridHeight
}

func (g *Gridder) getCellCenter(row, column int) *gg.Point {
	columns := float64(g.gridConfig.GetColumns())
	rows := float64(g.gridConfig.GetRows())

	cellWidth, cellHeight := g.getCellDimensions()
	gridWidth, gridHeight := g.getGridDimensions()

	x := float64(column)*(gridWidth/columns) + cellWidth/2
	y := float64(row)*(gridHeight/rows) + cellHeight/2
	return &gg.Point{X: x, Y: y}
}

func (g *Gridder) verifyInBounds(row, column int) error {
	columns := g.gridConfig.GetColumns()
	rows := g.gridConfig.GetRows()
	if row < 0 || row >= rows || column < 0 || column >= columns {
		return errOutOfBounds
	}
	return nil
}
