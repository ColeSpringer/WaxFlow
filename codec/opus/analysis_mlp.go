package opus

// Tonality analyser MLP, ported from libopus src/mlp.c and src/mlp.h. The
// int8 layer weights live in silk_enc_tables_gen.go (mechanically extracted
// from src/mlp_data.c). The network is a 25-input dense layer, a 24-unit
// GRU, and a 2-output sigmoid dense layer producing the music and activity
// probabilities.

const (
	mlpWeightsScale = 1.0 / 128
	mlpMaxNeurons   = 32
)

type analysisDenseLayer struct {
	bias         []int8
	inputWeights []int8
	nbInputs     int
	nbNeurons    int
	sigmoid      bool
}

type analysisGRULayer struct {
	bias             []int8
	inputWeights     []int8
	recurrentWeights []int8
	nbInputs         int
	nbNeurons        int
}

var analysisLayer0 = analysisDenseLayer{
	bias:         analysis_layer0_bias,
	inputWeights: analysis_layer0_weights,
	nbInputs:     25,
	nbNeurons:    32,
}

var analysisLayer1 = analysisGRULayer{
	bias:             analysis_layer1_bias,
	inputWeights:     analysis_layer1_weights,
	recurrentWeights: analysis_layer1_recur_weights,
	nbInputs:         32,
	nbNeurons:        24,
}

var analysisLayer2 = analysisDenseLayer{
	bias:         analysis_layer2_bias,
	inputWeights: analysis_layer2_weights,
	nbInputs:     24,
	nbNeurons:    2,
	sigmoid:      true,
}

// tansigApprox is the rational tanh approximation (mlp.c tansig_approx).
func tansigApprox(x float32) float32 {
	const (
		n0 = 952.52801514
		n1 = 96.39235687
		n2 = 0.60863042
		d0 = 952.72399902
		d1 = 413.36801147
		d2 = 11.88600922
	)
	x2 := x * x
	num := (n2*x2+n1)*x2 + n0
	den := (d2*x2+d1)*x2 + d0
	num = num * x / den
	if num < -1 {
		return -1
	}
	if num > 1 {
		return 1
	}
	return num
}

func sigmoidApprox(x float32) float32 {
	return 0.5 + 0.5*tansigApprox(0.5*x)
}

// gemmAccum accumulates weights*x into out; weights are column-major with the
// given stride (mlp.c gemm_accum).
func gemmAccum(out []float32, weights []int8, rows, cols, colStride int, x []float32) {
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			out[i] += float32(weights[j*colStride+i]) * x[j]
		}
	}
}

// computeDense evaluates a dense layer (analysis_compute_dense).
func (l *analysisDenseLayer) compute(output, input []float32) {
	N := l.nbNeurons
	for i := 0; i < N; i++ {
		output[i] = float32(l.bias[i])
	}
	gemmAccum(output, l.inputWeights, N, l.nbInputs, N, input)
	for i := 0; i < N; i++ {
		output[i] *= mlpWeightsScale
	}
	if l.sigmoid {
		for i := 0; i < N; i++ {
			output[i] = sigmoidApprox(output[i])
		}
	} else {
		for i := 0; i < N; i++ {
			output[i] = tansigApprox(output[i])
		}
	}
}

// computeGRU evaluates one GRU step in place on state (analysis_compute_gru).
func (g *analysisGRULayer) compute(state, input []float32) {
	M := g.nbInputs
	N := g.nbNeurons
	stride := 3 * N
	var tmp, z, r, h [mlpMaxNeurons]float32

	// Update gate.
	for i := 0; i < N; i++ {
		z[i] = float32(g.bias[i])
	}
	gemmAccum(z[:], g.inputWeights, N, M, stride, input)
	gemmAccum(z[:], g.recurrentWeights, N, N, stride, state)
	for i := 0; i < N; i++ {
		z[i] = sigmoidApprox(mlpWeightsScale * z[i])
	}

	// Reset gate.
	for i := 0; i < N; i++ {
		r[i] = float32(g.bias[N+i])
	}
	gemmAccum(r[:], g.inputWeights[N:], N, M, stride, input)
	gemmAccum(r[:], g.recurrentWeights[N:], N, N, stride, state)
	for i := 0; i < N; i++ {
		r[i] = sigmoidApprox(mlpWeightsScale * r[i])
	}

	// Output.
	for i := 0; i < N; i++ {
		h[i] = float32(g.bias[2*N+i])
	}
	for i := 0; i < N; i++ {
		tmp[i] = state[i] * r[i]
	}
	gemmAccum(h[:], g.inputWeights[2*N:], N, M, stride, input)
	gemmAccum(h[:], g.recurrentWeights[2*N:], N, N, stride, tmp[:])
	for i := 0; i < N; i++ {
		state[i] = z[i]*state[i] + (1-z[i])*tansigApprox(mlpWeightsScale*h[i])
	}
}
