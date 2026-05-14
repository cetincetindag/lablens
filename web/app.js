const valueFormat = new Intl.NumberFormat("en-US", { maximumFractionDigits: 1 });

const chartTextColor = "oklch(0.84 0.02 80)";
const chartLineColor = "oklch(0.7 0.075 72)";
const chartGridColor = "oklch(0.4 0.015 80)";

let cpuChart;
let memoryChart;
let refreshInFlight = false;

function metricElement(id) {
  return document.getElementById(id);
}

function prefersReducedMotion() {
  return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
}

function animateOnce(element, className, durationMs = 320) {
  if (!element || prefersReducedMotion()) {
    return;
  }
  element.classList.remove(className);
  void element.offsetWidth;
  element.classList.add(className);
  window.setTimeout(() => element.classList.remove(className), durationMs);
}

function setMetricValue(id, value, suffix = "") {
  const element = metricElement(id);
  if (!element) {
    return;
  }
  const nextValue = `${valueFormat.format(value)}${suffix}`;
  if (element.textContent !== nextValue) {
    element.textContent = nextValue;
    animateOnce(element, "valuePulse");
    return;
  }
  element.textContent = nextValue;
}

function setText(id, text) {
  const element = metricElement(id);
  if (!element) {
    return;
  }
  element.textContent = text;
}

function setLiveState(isOnline) {
  const badge = metricElement("live-badge");
  if (!badge) {
    return;
  }

  badge.classList.toggle("is-offline", !isOnline);
  badge.textContent = isOnline ? "LIVE" : "OFFLINE";

  const dot = document.createElement("span");
  dot.className = "liveDot";
  dot.setAttribute("aria-hidden", "true");
  badge.prepend(dot);
}

function setSyncState(isSyncing) {
  const indicator = metricElement("refresh-indicator");
  if (!indicator) {
    return;
  }
  indicator.classList.toggle("is-syncing", isSyncing);
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
      animation: false,
      responsive: true,
      maintainAspectRatio: false,
      resizeDelay: 150,
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

function updateNameList(listId, countId, values, emptyText) {
  const listElement = document.getElementById(listId);
  const countElement = document.getElementById(countId);
  if (!listElement || !countElement) {
    return;
  }

  countElement.textContent = `${values.length}`;
  if (!values.length) {
    listElement.innerHTML = `<li class="mutedText">${emptyText}</li>`;
    return;
  }

  listElement.innerHTML = values.map((value) => `<li>${value}</li>`).join("");
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
  updateNameList("nodes-list", "nodes-list-count", [], "Waiting for metrics...");
  updateNameList("apps-list", "apps-list-count", [], "Waiting for metrics...");
  updateNameList("pods-list", "pods-list-count", [], "Waiting for metrics...");
  setLiveState(false);
  setText("last-updated", "Waiting for metrics...");
  setText("refresh-indicator", "Auto-refreshes every 5 seconds · waiting for successful sync");
}

async function refresh() {
  if (refreshInFlight) {
    return;
  }
  refreshInFlight = true;
  setSyncState(true);

  try {
    const data = await loadOverview();
    setMetricValue("metric-cpu", data.summary.cpuPercent, "%");
    setMetricValue("metric-memory", data.summary.memoryPercent, "%");
    setMetricValue("metric-storage", data.summary.storagePercent, "%");
    setMetricValue("metric-nodes", data.summary.nodes);
    setMetricValue("metric-pods", data.workloads.podsRunning);
    setMetricValue("metric-namespaces", data.workloads.namespaces);

    updateNodeTable(data.nodeStats);
    const resources = data.resources ?? { nodeNames: [], appNames: [], podNames: [] };
    updateNameList("nodes-list", "nodes-list-count", resources.nodeNames, "No nodes found");
    updateNameList("apps-list", "apps-list-count", resources.appNames, "No running apps found");
    updateNameList("pods-list", "pods-list-count", resources.podNames, "No running pods found");

    updateTrendChart(cpuChart, data.trends.cpuUsage);
    updateTrendChart(memoryChart, data.trends.memoryUsage);

    const updatedAt = new Date(data.updatedAt);
    const updatedText = updatedAt.toLocaleTimeString();
    setLiveState(true);
    setText("last-updated", `Updated ${updatedText}`);
    setText("refresh-indicator", `Auto-refreshes every 5 seconds · last sync ${updatedText}`);
  } catch (error) {
    setErrorState(error);
  } finally {
    refreshInFlight = false;
    setSyncState(false);
  }
}

function init() {
  document.body.classList.add("is-loaded");
  setLiveState(true);
  cpuChart = createTrendChart("cpu-trend", "CPU Usage");
  memoryChart = createTrendChart("memory-trend", "Memory Usage");
  refresh();
  setInterval(refresh, 5000);
}

window.addEventListener("DOMContentLoaded", init);
