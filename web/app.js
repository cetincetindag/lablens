const valueFormat = new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 });

const chartTextColor = "oklch(0.84 0.02 80)";
const chartLineColor = "oklch(0.7 0.075 72)";
const chartGridColor = "oklch(0.4 0.015 80)";

let cpuChart;
let memoryChart;

function metricElement(id) {
  return document.getElementById(id);
}

function setMetricValue(id, value, suffix = "") {
  const element = metricElement(id);
  if (!element) {
    return;
  }
  element.textContent = `${valueFormat.format(value)}${suffix}`;
}

function createTrendChart(canvasId, label) {
  const canvas = document.getElementById(canvasId);
  const context = canvas.getContext("2d");
  return new Chart(context, {
    type: "line",
    data: {
      labels: [],
      datasets: [
        {
          label,
          data: [],
          borderColor: chartLineColor,
          borderWidth: 1.8,
          fill: false,
          tension: 0.28,
          pointRadius: 0,
        },
      ],
    },
    options: {
      responsive: true,
      maintainAspectRatio: false,
      scales: {
        x: {
          ticks: {
            color: chartTextColor,
            maxTicksLimit: 6,
          },
          grid: {
            color: chartGridColor,
          },
        },
        y: {
          min: 0,
          max: 100,
          ticks: {
            callback: (v) => `${v}%`,
            color: chartTextColor,
          },
          grid: {
            color: chartGridColor,
          },
        },
      },
      plugins: {
        legend: {
          display: false,
        },
      },
    },
  });
}

function updateTrendChart(chart, points) {
  const labels = points.map((p) =>
    new Date(p.timestamp * 1000).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }),
  );
  const values = points.map((p) => Number(p.value.toFixed(2)));
  chart.data.labels = labels;
  chart.data.datasets[0].data = values;
  chart.update("none");
}

function updateNodeTable(nodes) {
  const body = document.getElementById("nodes-body");
  if (!body) {
    return;
  }

  if (!nodes.length) {
    body.innerHTML = `<tr><td colspan="4" class="mutedText">No node metrics found</td></tr>`;
    return;
  }

  body.innerHTML = nodes
    .map(
      (node) => `<tr>
        <td>${node.name}</td>
        <td>${valueFormat.format(node.cpuPercent)}%</td>
        <td>${valueFormat.format(node.memoryPercent)}%</td>
        <td>${valueFormat.format(node.storagePercent)}%</td>
      </tr>`,
    )
    .join("");
}

async function loadOverview() {
  const response = await fetch("/api/overview", { cache: "no-store" });
  if (!response.ok) {
    throw new Error(await response.text());
  }
  return response.json();
}

function setErrorState(error) {
  const nodesBody = document.getElementById("nodes-body");
  if (nodesBody) {
    nodesBody.innerHTML = `<tr><td colspan="4" class="mutedText">${error.message}</td></tr>`;
  }
  metricElement("last-updated").textContent = "Waiting for metrics...";
}

async function refresh() {
  try {
    const data = await loadOverview();
    setMetricValue("metric-cpu", data.summary.cpuPercent, "%");
    setMetricValue("metric-memory", data.summary.memoryPercent, "%");
    setMetricValue("metric-storage", data.summary.storagePercent, "%");
    setMetricValue("metric-nodes", data.summary.nodes);
    setMetricValue("metric-pods", data.workloads.podsRunning);
    setMetricValue("metric-namespaces", data.workloads.namespaces);
    setMetricValue("metric-deployments", data.workloads.deploymentsAvailable);

    updateNodeTable(data.nodeStats);

    updateTrendChart(cpuChart, data.trends.cpuUsage);
    updateTrendChart(memoryChart, data.trends.memoryUsage);

    const updatedAt = new Date(data.updatedAt);
    metricElement("last-updated").textContent = `Updated ${updatedAt.toLocaleTimeString()}`;
  } catch (error) {
    setErrorState(error);
  }
}

function init() {
  cpuChart = createTrendChart("cpu-trend", "CPU Usage");
  memoryChart = createTrendChart("memory-trend", "Memory Usage");
  refresh();
  setInterval(refresh, 15000);
}

window.addEventListener("DOMContentLoaded", init);
